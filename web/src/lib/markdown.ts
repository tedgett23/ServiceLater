import DOMPurify from 'dompurify';
import { marked } from 'marked';

/**
 * An article's Markdown as HTML that is safe to put in the page: marked renders it, DOMPurify removes anything that
 * could run (scripts, event handlers, javascript: links). Links open in a new tab, since an article is often read
 * beside a ticket.
 */
export function renderMarkdown(md: string): string {
	const html = marked.parse(md, { async: false, gfm: true, breaks: false }) as string;
	const clean = DOMPurify.sanitize(html, { USE_PROFILES: { html: true }, FORBID_TAGS: ['style', 'form', 'input', 'button'] });
	const doc = new DOMParser().parseFromString(clean, 'text/html');
	for (const a of doc.querySelectorAll('a[href]')) {
		a.setAttribute('target', '_blank');
		a.setAttribute('rel', 'noopener noreferrer');
	}
	return doc.body.innerHTML;
}
