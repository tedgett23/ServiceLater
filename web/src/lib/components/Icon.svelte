<script lang="ts" module>
	// Simple line icons on a 24 grid, drawn with the text colour.
	const paths = {
		tickets: 'M3 13l3-8h12l3 8v6H3z M3 13h5l1 3h6l1-3h5',
		calendar: 'M4 6h16v14H4z M4 10h16 M8 3v4 M16 3v4',
		catalog: 'M4 4h16v16H4z M12 8v8 M8 12h8',
		knowledge: 'M5 4h10a3 3 0 0 1 3 3v13H8a3 3 0 0 1-3-3z M5 17a3 3 0 0 1 3-3h10',
		cmdb: 'M4 5h16v6H4z M4 13h16v6H4z M8 8h.01 M8 16h.01',
		problems: 'M12 4l9 16H3z M12 10v4 M12 17h.01',
		workflows: 'M5 5h5v5H5z M14 14h5v5h-5z M10 7.5h6.5V14',
		runs: 'M7 5l11 7-11 7z',
		admin: 'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z M12 3v3 M12 18v3 M3 12h3 M18 12h3 M5.6 5.6l2.1 2.1 M16.3 16.3l2.1 2.1 M5.6 18.4l2.1-2.1 M16.3 7.7l2.1-2.1',
		approvals: 'M12 3l8 3v6c0 4.5-3.2 7.6-8 9-4.8-1.4-8-4.5-8-9V6z M8.5 12l2.5 2.5 4.5-5',
		bell: 'M6 16v-5a6 6 0 0 1 12 0v5l2 2H4z M10 20a2 2 0 0 0 4 0',
		plus: 'M12 5v14 M5 12h14',
		search: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z M16 16l4 4',
		signout: 'M14 4h5v16h-5 M10 8l-4 4 4 4 M6 12h10',
		sidebar: 'M4 5h16v14H4z M9 5v14',
		back: 'M14 6l-6 6 6 6'
	} as const;
	export type IconName = keyof typeof paths;
</script>

<script lang="ts">
	let { name, size = 16 }: { name: IconName; size?: number } = $props();
</script>

<svg class="icon" width={size} height={size} viewBox="0 0 24 24" aria-hidden="true">
	<path d={paths[name]} />
</svg>

<style>
	.icon {
		flex: none;
		fill: none;
		stroke: currentColor;
		stroke-width: 1.7;
		stroke-linecap: round;
		stroke-linejoin: round;
	}
</style>
