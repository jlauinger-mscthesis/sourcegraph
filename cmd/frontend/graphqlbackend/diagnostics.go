package graphqlbackend

type Diagnostic interface {
	Type() string
	Data() JSONValue
}
