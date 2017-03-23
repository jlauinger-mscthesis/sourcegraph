package graphqlbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/opentracing-contrib/go-stdlib/nethttp"
	opentracing "github.com/opentracing/opentracing-go"

	"sourcegraph.com/sourcegraph/sourcegraph/api/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/env"
	"sourcegraph.com/sourcegraph/sourcegraph/services/backend"
	"sourcegraph.com/sourcegraph/sourcegraph/services/backend/internal/localstore"
	"sourcegraph.com/sourcegraph/sourcegraph/xlang/uri"
)

// A light wrapper around the search service. We implement the service here so
// that we can unmarshal the result directly into graphql resolvers.

var searcherURL = env.Get("SEARCHER_URL", "", "searcher server URL (eg http://localhost:3181)")

// patternInfo is the struct used by vscode pass on search queries.
type patternInfo struct {
	Pattern         string
	IsRegExp        bool
	IsWordMatch     bool
	IsCaseSensitive bool
	// We do not support IsMultiline
	//IsMultiline     bool
}

// FileMatch is the struct used by vscode to receive search results
type fileMatch struct {
	JPath        string       `json:"Path"`
	JLineMatches []*lineMatch `json:"LineMatches"`
}

func (fm *fileMatch) Path() string {
	return fm.JPath
}

func (fm *fileMatch) LineMatches() []*lineMatch {
	return fm.JLineMatches
}

// LineMatch is the struct used by vscode to receive search results for a line
type lineMatch struct {
	JPreview          string    `json:"Preview"`
	JLineNumber       int32     `json:"LineNumber"`
	JOffsetAndLengths [][]int32 `json:"OffsetAndLengths"`
}

func (lm *lineMatch) Preview() string {
	return lm.JPreview
}

func (lm *lineMatch) LineNumber() int32 {
	return lm.JLineNumber + 1
}

func (lm *lineMatch) OffsetAndLengths() [][]int32 {
	return lm.JOffsetAndLengths
}

func (r *commitResolver) TextSearch(ctx context.Context, info *patternInfo) ([]*fileMatch, error) {
	return textSearch(ctx, r.repo.URI, r.commit.CommitID, info)
}

func textSearch(ctx context.Context, repo, commit string, p *patternInfo) ([]*fileMatch, error) {
	if searcherURL == "" {
		return nil, errors.New("a searcher service has not been configured")
	}
	q := url.Values{
		"Repo":    []string{repo},
		"Commit":  []string{commit},
		"Pattern": []string{p.Pattern},
	}
	if p.IsRegExp {
		q.Set("IsRegExp", "true")
	}
	if p.IsWordMatch {
		q.Set("IsWordMatch", "true")
	}
	if p.IsCaseSensitive {
		q.Set("IsCaseSensitive", "true")
	}
	req, err := http.NewRequest("GET", searcherURL, nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = q.Encode()
	req = req.WithContext(ctx)

	req, ht := nethttp.TraceRequest(opentracing.GlobalTracer(), req,
		nethttp.OperationName("Searcher Client"),
		nethttp.ClientTrace(false))
	defer ht.Finish()

	client := &http.Client{Transport: &nethttp.Transport{}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("non-200 response: code=%d body=%s", resp.StatusCode, string(body))
	}

	var matches []*fileMatch
	err = json.NewDecoder(resp.Body).Decode(&matches)
	return matches, err
}

type repoMatch struct {
	uri         uri.URI
	lineMatches []*lineMatch
}

func (rm *repoMatch) LineMatches() []*lineMatch {
	return rm.lineMatches
}

func (rm *repoMatch) URI() string {
	return rm.uri.String()
}

func searchRepo(ctx context.Context, repoName string, info *patternInfo) ([]repoMatch, error) {
	repo, err := localstore.Repos.GetByURI(ctx, repoName)
	if err != nil {
		return nil, err
	}
	commit, err := backend.Repos.ResolveRev(ctx, &sourcegraph.ReposResolveRevOp{
		Repo: repo.ID,
	})
	fileMatches, err := textSearch(ctx, repoName, commit.CommitID, info)
	if err != nil {
		return nil, err
	}
	repoMatches := make([]repoMatch, len(fileMatches))
	for i, fm := range fileMatches {
		repoMatches[i].lineMatches = fm.JLineMatches
		uri, err := uri.Parse(repoName + "?" + commit.CommitID + "#" + fm.JPath)
		if err != nil {
			return nil, err
		}
		repoMatches[i].uri = *uri
	}
	return repoMatches, nil
}

// accumulate aggregates the results of a cross-repo search and sorts them by
// file, according to 1. the number of matches and 2. the repo/path.
func accumulate(responses <-chan []repoMatch, result chan<- []repoMatch) {
	var flattened []repoMatch
	for response := range responses {
		flattened = append(flattened, response...)
	}
	sort.Slice(flattened, func(i, j int) bool {
		a, b := len(flattened[i].lineMatches), len(flattened[j].lineMatches)
		if a != b {
			return a < b
		}
		return strings.Compare(flattened[i].uri.Path, flattened[j].uri.Path) < 0
	})
	result <- flattened
}

type repoSearchArgs struct {
	Info  patternInfo
	Repos []string
}

// SearchRepos searches a set of repos for a pattern.
func (r *currentUserResolver) SearchRepos(ctx context.Context, args *repoSearchArgs) ([]repoMatch, error) {
	ctx, cancel := context.WithCancel(ctx)
	responses := make(chan []repoMatch)
	result := make(chan []repoMatch)
	repositories := make(chan string)
	wg := sync.WaitGroup{}
	go accumulate(responses, result)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repo := range repositories {
				if _, ok := <-ctx.Done(); ok {
					return
				}
				rm, err := searchRepo(ctx, repo, &args.Info)
				if err != nil {
					cancel()
					return
				}
				responses <- rm
			}
		}()
	}
	for _, repo := range args.Repos {
		repositories <- repo
	}
	close(repositories)
	wg.Wait()
	close(responses)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	cancel()
	return <-result, nil
}
