<script lang="ts">
	import { page } from '$app/state';
	let { children } = $props();
</script>

<!-- A parent and its child share a route, but not a run snapshot or polling timer. -->
{#key page.params.id}{@render children()}{/key}
