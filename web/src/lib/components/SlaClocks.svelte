<script lang="ts">
	import { clock } from '$lib/clock.svelte';
	import { span, when } from '$lib/format';
	import type { SlaClock } from '$lib/types';

	/** A ticket's SLA clocks. Everything but the countdown to due_at comes from the server as it was fetched. */
	let { clocks }: { clocks: SlaClock[] } = $props();

	$effect(() => clock.watch());

	function state(c: SlaClock, now: number) {
		const used = `${span(c.used_ms)} of ${span(c.target_ms)} used`;
		const share = Math.min(1, c.used_ms / c.target_ms);
		if (c.stage === 'stopped') {
			return c.breached_at
				? { text: 'Missed', tone: 'bad', detail: used, share: 1 }
				: { text: 'Met', tone: 'good', detail: `Took ${span(c.used_ms)} of ${span(c.target_ms)}`, share };
		}
		if (c.breached_at) {
			const over = now - new Date(c.breached_at).getTime();
			return { text: `Overdue by ${span(over)}`, tone: 'bad', detail: `Target was ${span(c.target_ms)}`, share: 1 };
		}
		if (c.stage === 'paused') return { text: 'Paused', tone: 'quiet', detail: used, share };
		const left = c.due_at ? new Date(c.due_at).getTime() - now : 0;
		return {
			text: left > 60_000 ? `Due in ${span(left)}` : 'Due now',
			tone: share >= 0.75 || left < 3_600_000 ? 'soon' : 'ok',
			detail: c.due_at ? `${used} · due ${when(c.due_at)}` : used,
			share
		};
	}
</script>

<ul class="clocks">
	{#each clocks as c (c.id)}
		{@const s = state(c, clock.now)}
		<li class={s.tone}>
			<div class="line">
				<span class="label">{c.label}</span>
				<span class="state">{s.text}</span>
			</div>
			<div class="bar" aria-hidden="true"><span style="width: {Math.round(s.share * 100)}%"></span></div>
			<p class="detail">{s.detail}{c.calendar ? '' : ' · every hour counts'}</p>
			{#if c.problem}<p class="problem">{c.problem}</p>{/if}
		</li>
	{/each}
</ul>

<style>
	.clocks {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.75rem;
		width: 100%;
	}
	.line {
		display: flex;
		justify-content: space-between;
		gap: 0.75rem;
		font-size: 0.9rem;
	}
	.label {
		font-weight: 600;
	}
	.state {
		white-space: nowrap;
		font-weight: 600;
	}
	.bar {
		height: 4px;
		margin: 0.3rem 0 0.25rem;
		background: var(--rule);
		border-radius: 2px;
		overflow: hidden;
	}
	.bar span {
		display: block;
		height: 100%;
		background: var(--ink-2);
	}
	.detail,
	.problem {
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.problem {
		margin-top: 0.2rem;
		padding-left: 0.5rem;
		border-left: 3px solid var(--marker);
	}
	/* Yellow means look here: a quarter or less of the target left. Red: the target is gone. */
	.soon .state {
		padding: 0 0.3rem;
		background: var(--marker);
		color: var(--on-marker);
		border-radius: 2px;
	}
	.soon .bar span {
		background: var(--marker);
	}
	.bad .state {
		color: var(--red);
	}
	.bad .bar span {
		background: var(--red);
	}
	.good .state,
	.quiet .state {
		color: var(--ink-2);
	}
</style>
