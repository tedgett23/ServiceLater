/**
 * CodeMirror for SQL queries (the workbench, /queries): Postgres highlighting, completion of the views and their
 * columns (with what each column holds), Ctrl/Cmd+Enter to run, and the server's problems marked where they are.
 * Imported only on the workbench, so other pages never load it.
 */
import { autocompletion, closeCompletion, type Completion } from '@codemirror/autocomplete';
import { PostgreSQL, sql } from '@codemirror/lang-sql';
import { syntaxHighlighting } from '@codemirror/language';
import { lintGutter, setDiagnostics } from '@codemirror/lint';
import { Compartment, EditorState, Prec } from '@codemirror/state';
import { EditorView, keymap } from '@codemirror/view';
import { basicSetup } from 'codemirror';
import { highlight, theme } from './codemirror';

/** A view as completion offers it: its name, what it is, and its columns. */
export interface SchemaView {
	name: string;
	description: string;
	columns: { name: string; type: string; description?: string }[];
}

export interface SqlView {
	/** Marks a problem at a line and column from 1 (0, 0: the query as a whole), or clears it with null. */
	setProblem(p: { line: number; column: number; message: string } | null): void;
	setSchema(views: SchemaView[]): void;
	/** Replaces the text (opening a saved view or a past query). */
	setText(text: string): void;
	/** Puts text where the cursor is (a view's or column's name, picked from the list). */
	insert(text: string): void;
	focus(): void;
	destroy(): void;
}

function language(views: SchemaView[]) {
	const schema: Record<string, Completion[]> = {};
	const tables: Completion[] = [];
	for (const v of views) {
		schema[v.name] = v.columns.map((c) => ({ label: c.name, type: 'property', detail: c.type, info: c.description || undefined }));
		tables.push({ label: v.name, type: 'type', info: v.description || undefined });
	}
	return sql({ dialect: PostgreSQL, schema, tables, upperCaseKeywords: true });
}

/** Creates an editor in parent. onchange hears every edit; onrun is Ctrl/Cmd+Enter. */
export function createSqlEditor(
	parent: HTMLElement,
	doc: string,
	views: SchemaView[],
	label: string,
	onchange: (text: string) => void,
	onrun: () => void
): SqlView {
	const lang = new Compartment();
	const view = new EditorView({
		parent,
		state: EditorState.create({
			doc,
			extensions: [
				Prec.highest(
					keymap.of([
						{
							key: 'Mod-Enter',
							run: (v) => {
								closeCompletion(v);
								onrun();
								return true;
							}
						}
					])
				),
				basicSetup,
				lang.of(language(views)),
				theme,
				EditorView.theme({ '.cm-scroller': { minHeight: '12rem', maxHeight: '24rem' } }),
				syntaxHighlighting(highlight),
				autocompletion({ icons: false }),
				lintGutter(),
				EditorView.contentAttributes.of({ 'aria-label': label }),
				EditorView.updateListener.of((u) => {
					if (u.docChanged) onchange(u.state.doc.toString());
				})
			]
		})
	});
	return {
		setProblem(p) {
			if (!p) {
				view.dispatch(setDiagnostics(view.state, []));
				return;
			}
			const d = view.state.doc;
			let from = 0;
			let to = d.length;
			if (p.line > 0) {
				const line = d.line(Math.min(p.line, d.lines));
				from = Math.min(line.from + Math.max(p.column - 1, 0), line.to);
				// Mark the word that starts there, or the rest of the line.
				const rest = d.sliceString(from, line.to);
				const word = /^[\w.$]+/.exec(rest);
				to = word ? from + word[0].length : line.to;
				if (to === from) to = Math.min(from + 1, d.length);
			}
			view.dispatch(setDiagnostics(view.state, [{ from, to, severity: 'error', message: p.message }]));
		},
		setSchema(views) {
			view.dispatch({ effects: lang.reconfigure(language(views)) });
		},
		setText(text) {
			view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
		},
		insert(text) {
			const { from, to } = view.state.selection.main;
			view.dispatch({ changes: { from, to, insert: text }, selection: { anchor: from + text.length } });
			view.focus();
		},
		focus() {
			view.focus();
		},
		destroy() {
			view.destroy();
		}
	};
}
