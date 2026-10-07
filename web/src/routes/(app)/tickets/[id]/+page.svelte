<script lang="ts">
	import TicketDetail from '$lib/components/TicketDetail.svelte';
	import { notifications } from '$lib/notifications.svelte';

	let { data } = $props();

	// Opening a ticket answers its notifications. Also runs when they arrive while it is open.
	$effect(() => {
		void notifications.readTask(data.ticket.id);
	});
</script>

<svelte:head><title>{data.ticket.reference} {data.ticket.title} – ServiceLater</title></svelte:head>

<!-- Keyed so moving to another ticket starts from that ticket's data, not the previous one's edits. -->
{#key data.ticket.id}
	<TicketDetail ticket={data.ticket} comments={data.comments} />
{/key}
