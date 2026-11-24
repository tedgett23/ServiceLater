<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import favicon from '$lib/assets/favicon.svg';
	import { instance } from '$lib/instance.svelte';

	let { children } = $props();
	// Any page, the sign-in page included, says when the instance is being restored.
	onMount(() => void instance.load());
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
	<title>ServiceLater</title>
</svelte:head>

{@render children()}

{#if instance.maintenance}
	<div class="maintenance" role="alertdialog" aria-modal="true" aria-labelledby="maint-h" aria-describedby="maint-p">
		<div class="maint-box">
			{#if instance.maintenance.operation === 'clone'}
				<h2 id="maint-h">Loading a clone</h2>
				<p id="maint-p">
					{instance.maintenance.by || 'An admin'} is replacing the records of {instance.name || 'this instance'} with a copy of
					production. Everything is unavailable until it finishes, usually in a few minutes. You will be asked to sign in again.
				</p>
			{:else}
				<h2 id="maint-h">Restoring a snapshot</h2>
				<p id="maint-p">
					{instance.maintenance.by || 'An admin'} is restoring {instance.name || 'this instance'} from a snapshot. Everything is
					unavailable until it finishes, usually in a few minutes. You will be asked to sign in again.
				</p>
			{/if}
			<p class="maint-wait" aria-live="polite">
				{instance.maintenance.status === 'pending' ? 'Waiting for the worker…' : instance.maintenance.operation === 'clone' ? 'Loading…' : 'Restoring…'}
			</p>
		</div>
	</div>
{/if}

<style>
	.maintenance {
		position: fixed;
		inset: 0;
		z-index: 100;
		display: grid;
		place-items: center;
		padding: 1rem;
		background: color-mix(in srgb, var(--ground) 85%, transparent);
	}
	.maint-box {
		max-width: 28rem;
		padding: 1.25rem 1.5rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		box-shadow: var(--shadow-pop);
	}
	.maint-box h2 {
		margin: 0 0 0.5rem;
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.maint-box p {
		margin: 0 0 0.5rem;
	}
	.maint-wait {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
</style>
