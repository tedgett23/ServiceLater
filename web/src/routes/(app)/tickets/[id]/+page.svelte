<script lang="ts">
	import TicketDetail from '$lib/components/TicketDetail.svelte';

	let { data } = $props();
</script>

<svelte:head><title>#{data.ticket.number} {data.ticket.title} – ServiceLater</title></svelte:head>

<!-- Keyed so moving to another ticket starts from that ticket's data, not the previous one's edits. -->
{#key data.ticket.id}
	<TicketDetail ticket={data.ticket} comments={data.comments} />
{/key}
