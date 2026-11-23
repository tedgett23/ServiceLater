/**
 * CodeMirror set up for ServiceLater's Starlark scripts: our tokens for colour (so light and dark follow the theme),
 * Python highlighting (Starlark is a subset), completion of the script functions and record fields, and error marks.
 * Imported only where a script is edited, so other pages never load it.
 */
import { autocompletion, type CompletionContext, type CompletionResult } from '@codemirror/autocomplete';
import { python } from '@codemirror/lang-python';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { lintGutter, setDiagnostics } from '@codemirror/lint';
import { Compartment, EditorState } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { tags as t } from '@lezer/highlight';
import { basicSetup } from 'codemirror';

/** The editor's look, in our tokens (shared with the SQL editor). */
export const theme = EditorView.theme({
	'&': { fontSize: '13px', background: 'var(--raised)', color: 'var(--ink)', border: '1px solid var(--rule)', borderRadius: '8px' },
	'.cm-scroller': { fontFamily: 'var(--font-mono)', lineHeight: '1.6', minHeight: '9rem', maxHeight: '28rem' },
	'.cm-content': { caretColor: 'var(--ink)' },
	'.cm-gutters': { background: 'var(--surface)', color: 'var(--ink-2)', border: 'none', borderRight: '1px solid var(--rule)', borderRadius: '8px 0 0 8px' },
	'.cm-activeLine': { background: 'color-mix(in srgb, var(--blue) 6%, transparent)' },
	'.cm-activeLineGutter': { background: 'transparent', color: 'var(--ink)' },
	'&.cm-focused': { outline: '2px solid var(--focus)', outlineOffset: '-1px' },
	'.cm-cursor': { borderLeftColor: 'var(--ink)' },
	'.cm-tooltip': { background: 'var(--raised)', color: 'var(--ink)', border: '1px solid var(--rule)', borderRadius: '8px', boxShadow: 'var(--shadow-pop)' },
	'.cm-tooltip-autocomplete ul li[aria-selected]': { background: 'var(--blue)', color: 'var(--on-blue)' },
	'.cm-completionDetail': { color: 'inherit', opacity: '0.7', fontStyle: 'normal', marginLeft: '1em' },
	'.cm-selectionBackground, &.cm-focused .cm-selectionBackground': { background: 'color-mix(in srgb, var(--blue) 22%, transparent) !important' },
	'.cm-matchingBracket': { background: 'var(--marker-soft)', outline: 'none' },
	'.cm-panels': { background: 'var(--surface)', color: 'var(--ink)' }
});

/** Code colours, in our tokens. */
export const highlight = HighlightStyle.define([
	{ tag: t.keyword, color: 'var(--blue)', fontWeight: '600' },
	{ tag: t.string, color: 'var(--green)' },
	{ tag: t.comment, color: 'var(--ink-2)', fontStyle: 'italic' },
	{ tag: [t.number, t.bool, t.null], color: 'var(--marker-ink)' },
	{ tag: t.definition(t.variableName), fontWeight: '600' },
	{ tag: t.function(t.variableName), color: 'var(--red)' }
]);

/** What a script can call or read, as completion offers it. */
const builtins: { label: string; detail: string; info: string; type: string }[] = [
	{ label: 'record', detail: 'record.title, record.fields["x"]…', info: 'The record this runs for', type: 'variable' },
	{ label: 'vars', detail: 'vars["name"]', info: "The workflow's variables", type: 'variable' },
	{ label: 'user', detail: 'user.name, user.id', info: 'The person it runs for', type: 'variable' },
	{ label: 'now', detail: 'time', info: 'The time now', type: 'variable' },
	{ label: 'children', detail: 'children(record, open=False)', info: 'The records under one (a request’s tasks)', type: 'function' },
	{ label: 'get', detail: 'get("INC42")', info: 'A record by its reference, or None', type: 'function' },
	{ label: 'count', detail: 'count("tasks", {"status": "open"})', info: 'How many records match, as rule lookups count', type: 'function' },
	{ label: 'update', detail: 'update(record, {"field": value}, comment=None)', info: 'Change a record when the script ends; "status" moves it', type: 'function' },
	{ label: 'comment', detail: 'comment(record, "text", internal=False)', info: 'Add a comment when the script ends', type: 'function' },
	{ label: 'abort', detail: 'abort("message")', info: 'Stop: refuse the save, or fail the step', type: 'function' },
	{ label: 'print', detail: 'print(...)', info: 'Written to the run’s log', type: 'function' }
];

const recordFields = [
	'id', 'type', 'reference', 'number', 'title', 'description', 'status', 'state_category', 'priority', 'requester_id', 'requester_name',
	'assignee_id', 'assignee_name', 'assignment_group', 'assignment_group_id', 'created_at', 'updated_at', 'resolution', 'fields', 'cis',
	'parent_id', 'open_tasks', 'catalog_item', 'requester_manager_id', 'problem_id', 'open_incidents', 'change_type', 'risk', 'impact',
	'planned_start', 'planned_end', 'implementation_plan', 'backout_plan', 'test_plan'
];

function complete(ctx: CompletionContext): CompletionResult | null {
	const member = ctx.matchBefore(/\b(record|task)\.\w*/);
	if (member) {
		const dot = member.text.indexOf('.') + 1;
		return { from: member.from + dot, options: recordFields.map((label) => ({ label, type: 'property' })) };
	}
	const word = ctx.matchBefore(/\w*/);
	if (!word || (word.from === word.to && !ctx.explicit)) return null;
	return { from: word.from, options: builtins };
}

export interface Problem {
	line: number;
	message: string;
}

export interface ScriptView {
	setProblems(list: Problem[]): void;
	setReadOnly(on: boolean): void;
	destroy(): void;
}

/** Creates an editor in parent, telling onchange of every edit. */
export function createScriptEditor(parent: HTMLElement, doc: string, readOnly: boolean, label: string, onchange: (text: string) => void): ScriptView {
	const ro = new Compartment();
	const view = new EditorView({
		parent,
		state: EditorState.create({
			doc,
			extensions: [
				basicSetup,
				python(),
				theme,
				syntaxHighlighting(highlight),
				autocompletion({ override: [complete] }),
				lintGutter(),
				EditorState.tabSize.of(4),
				ro.of([EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]),
				EditorView.contentAttributes.of({ 'aria-label': label }),
				EditorView.updateListener.of((u) => {
					if (u.docChanged) onchange(u.state.doc.toString());
				})
			]
		})
	});
	return {
		setProblems(list) {
			const doc = view.state.doc;
			const diags = list.map((p) => {
				const line = doc.line(Math.min(Math.max(p.line || 1, 1), doc.lines));
				return { from: line.from, to: Math.max(line.to, line.from), severity: 'error' as const, message: p.message };
			});
			view.dispatch(setDiagnostics(view.state, diags));
		},
		setReadOnly(on) {
			view.dispatch({ effects: ro.reconfigure([EditorState.readOnly.of(on), EditorView.editable.of(!on)]) });
		},
		destroy() {
			view.destroy();
		}
	};
}
