<script lang="ts">
	import { page } from '$app/state';
</script>

<div class="empty">
	{#if page.status === 404}
		<p><strong>This ticket doesn’t exist, or you don’t have access to it.</strong></p>
		<p>Check the number, or pick another ticket from the queue.</p>
	{:else}
		<p><strong>Couldn’t load this ticket.</strong></p>
		<p>Check your connection, then select the ticket again.</p>
	{/if}
</div>

<style>
	.empty {
		height: 100%;
		display: grid;
		place-content: center;
		gap: 0.4rem;
		padding: 2rem;
		text-align: center;
		color: var(--ink-2);
	}
	strong {
		color: var(--ink);
	}
</style>
