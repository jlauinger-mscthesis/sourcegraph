import * as moment from 'moment';
import { BlameState, contextKey, store } from 'sourcegraph/blame/store';

function limitString(s: string, n: number, dotdotdot: boolean): string {
    if (s.length > n) {
        if (dotdotdot) {
            return s.substring(0, n - 1) + '…';
        }
        return s.substring(0, n);
    }
    return s;
}

/**
 * setLineBlameContent sets the given line's blame content.
 */
function setLineBlameContent(line: number, blameContent: string): void {
    // Remove blame class from all other lines.
    const currentlyBlamed = document.querySelectorAll('#blob-table td:first-child>.blame');
    for (const blame of currentlyBlamed) {
        blame.parentNode!.removeChild(blame);
    }

    if (line > 0) {
        // Add blame element to the target line's code cell.
        const cells = document.querySelectorAll('#blob-table td:first-child');
        const cell = cells[line - 1];
        const blame = document.createElement('span');
        blame.classList.add('blame');
        blame.setAttribute('data-blame', blameContent);
        cell.appendChild(blame);
    }
}

store.subscribe((state: BlameState) => {
    state = store.getValue();

    // Clear the blame content on whatever line it was already on.
    setLineBlameContent(-1, '');

    if (!state.context) {
        return;
    }
    const hunks = state.hunksByLoc.get(contextKey(state.context));
    if (!hunks) {
        if (state.displayLoading) {
            setLineBlameContent(state.context.line, 'loading ◌');
        }
        return;
    }

    const timeSince = moment(hunks[0].author.date, 'YYYY-MM-DD HH:mm:ss ZZ UTC').fromNow();
    const blameContent = `${hunks[0].author.person.name}, ${timeSince} • ${limitString(hunks[0].message, 80, true)} ${limitString(hunks[0].rev, 6, false)}`;

    setLineBlameContent(state.context.line, blameContent);
});
