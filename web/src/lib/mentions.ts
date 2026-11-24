/** A piece of a comment's text: plain, or one of the people it mentions (written as @ and their name). */
export type Part = { text: string; mention?: { id: string; name: string } };

/** Splits a comment into its text and its mentions, longest names first, so "@Ana Li" wins over "@Ana". */
export function mentionParts(body: string, mentions: { id: string; name: string }[] = []): Part[] {
	const names = [...mentions].sort((a, b) => b.name.length - a.name.length);
	const out: Part[] = [];
	let plain = '';
	for (let i = 0; i < body.length; ) {
		const m = body[i] === '@' ? names.find((n) => body.startsWith('@' + n.name, i)) : undefined;
		if (m) {
			if (plain) out.push({ text: plain });
			plain = '';
			out.push({ text: '@' + m.name, mention: m });
			i += m.name.length + 1;
		} else {
			plain += body[i++];
		}
	}
	if (plain) out.push({ text: plain });
	return out;
}

/** The mentions still named in the text, which are the ones to send. */
export function keptMentions(body: string, mentions: { id: string; name: string }[]): string[] {
	return mentions.filter((m) => body.includes('@' + m.name)).map((m) => m.id);
}
