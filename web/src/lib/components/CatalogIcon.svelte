<script lang="ts">
	/** A catalog item's picture, from the fixed set apps may name (apps.CatalogIcons), drawn here so nothing loads from an app. */
	let { name, size = 24 }: { name?: string; size?: number } = $props();
</script>

<svg viewBox="0 0 24 24" width={size} height={size} fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
	{#if name === 'laptop'}
		<rect x="4" y="5" width="16" height="11" rx="1.5" /><path d="M2 19h20" />
	{:else if name === 'phone'}
		<rect x="7" y="2.5" width="10" height="19" rx="2" /><path d="M11 18.5h2" />
	{:else if name === 'monitor'}
		<rect x="3" y="4" width="18" height="12" rx="1.5" /><path d="M9 20h6M12 16v4" />
	{:else if name === 'printer'}
		<path d="M7 9V3h10v6" /><rect x="3" y="9" width="18" height="8" rx="1.5" /><path d="M7 14h10v7H7z" />
	{:else if name === 'software'}
		<rect x="3" y="4" width="18" height="16" rx="2" /><path d="M3 9h18M9 12.5l-2.5 2.5L9 17.5M15 12.5l2.5 2.5-2.5 2.5" />
	{:else if name === 'key'}
		<circle cx="8" cy="15" r="4" /><path d="M11 12 20 3M17 6l3 3M14.5 8.5l2 2" />
	{:else if name === 'person'}
		<circle cx="12" cy="8" r="4" /><path d="M4 21c0-4 3.6-6.5 8-6.5s8 2.5 8 6.5" />
	{:else if name === 'network'}
		<circle cx="12" cy="5" r="2.5" /><circle cx="5" cy="19" r="2.5" /><circle cx="19" cy="19" r="2.5" /><path d="M12 7.5v4M12 11.5 6.5 17M12 11.5l5.5 5.5" />
	{:else if name === 'server'}
		<rect x="4" y="3" width="16" height="7" rx="1.5" /><rect x="4" y="14" width="16" height="7" rx="1.5" /><path d="M8 6.5h.01M8 17.5h.01" />
	{:else if name === 'building'}
		<path d="M4 21V4h11v17M15 9h5v12M2 21h20M8 8h3M8 12h3M8 16h3" />
	{:else if name === 'box'}
		<path d="m3 7 9-4 9 4v10l-9 4-9-4z" /><path d="m3 7 9 4 9-4M12 11v10" />
	{:else}
		<circle cx="12" cy="12" r="9" /><path d="M9.5 9.5a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .8-1 1.5V14M12 17.5h.01" />
	{/if}
</svg>
