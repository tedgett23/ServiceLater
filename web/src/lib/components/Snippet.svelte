<script lang="ts">
	/**
	 * A search snippet: plain text in which the server marked each match between ⸢ and ⸣. The marks become <mark>
	 * elements here, so no HTML from the server is ever trusted. Markdown marks are left out.
	 */
	let { text }: { text: string } = $props();

	/** The words of a piece of Markdown without its marks: a snippet is read, not rendered. */
	function plain(md: string) {
		return md
			.replace(/!?\[([^\]]*)\]\([^)]*\)?/g, '$1') // [text](url) and images: the text
			.replace(/^\s{0,3}(#{1,6}|>|[-*+]|\d+\.)\s+/gm, '') // heading, quote and list marks
			.replace(/`{3}[a-z]*|[*_]{2,3}|`|^\s*[-|:\s]{3,}$/gim, '') // code fences, bold, code, table rules
			.replace(/\s*\|\s*/g, ' ') // table cells
			.replace(/\s+/g, ' ')
			.trim();
	}
	const parts = $derived(
		plain(text).split(/(⸢[^⸣]*⸣)/).filter(Boolean).map((p) => (p.startsWith('⸢') ? { hit: true, s: p.slice(1, -1) } : { hit: false, s: p }))
	);
</script>

<span class="snippet">{#each parts as p, i (i)}{#if p.hit}<mark>{p.s}</mark>{:else}{p.s}{/if}{/each}</span>

<style>
	mark {
		background: color-mix(in srgb, var(--marker) 55%, transparent);
		color: inherit;
		border-radius: 2px;
		padding: 0 0.1em;
	}
</style>
