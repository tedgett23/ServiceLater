/** Highest positional parameter in SQL being edited, bounded by the API's limit. Consume tokens left to right:
 * comment delimiters inside literals (and quotes inside comments) have no effect. */
export function paramCount(sql: string): number {
	const tokens = /--[^\r\n]*|\/\*|[eE]'|'|"|\$(?:[\p{L}_][\p{L}\p{N}_]*)?\$|\$\d+|[\p{L}_][\p{L}\p{N}_$]*/gu;
	let n = 0;
	let match: RegExpExecArray | null;
	while ((match = tokens.exec(sql))) {
		const t = match[0];
		if (t === '/*') {
			let depth = 1;
			const delimiters = /\/\*|\*\//g;
			delimiters.lastIndex = tokens.lastIndex;
			while (depth && (match = delimiters.exec(sql))) depth += match[0] === '/*' ? 1 : -1;
			tokens.lastIndex = depth ? sql.length : delimiters.lastIndex;
		} else if (t === "'" || t === "E'" || t === "e'" || t === '"') {
			const quote = t.at(-1)!;
			let i = tokens.lastIndex;
			while (i < sql.length) {
				const ch = sql[i++];
				if (ch === '\\' && t.length === 2) i++; // E'...' escape
				else if (ch === quote) {
					if (sql[i] !== quote) break;
					i++; // doubled quote
				}
			}
			tokens.lastIndex = i;
		} else if (t.startsWith('$')) {
			if (t.endsWith('$')) {
				const end = sql.indexOf(t, tokens.lastIndex);
				tokens.lastIndex = end < 0 ? sql.length : end + t.length;
			} else n = Math.max(n, Math.min(20, Number(t.slice(1))));
		}
	}
	return n;
}
