<script lang="ts">
	// A person as their initials on a tint chosen by their name, so the same person is always the same colour.
	let { name, size = 'md' }: { name: string; size?: 'sm' | 'md' } = $props();

	const initials = $derived(
		name
			.split(/\s+/)
			.filter(Boolean)
			.slice(0, 2)
			.map((p) => p[0]!.toUpperCase())
			.join('')
	);
	const hue = $derived([232, 28, 152, 268, 200, 340][[...name].reduce((n, c) => n + c.charCodeAt(0), 0) % 6]);
</script>

<span class="avatar {size}" style="--h: {hue}" aria-hidden="true">{initials}</span>

<style>
	.avatar {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex: none;
		width: 1.75rem;
		height: 1.75rem;
		border-radius: 50%;
		background: light-dark(hsl(var(--h) 80% 93%), hsl(var(--h) 38% 22%));
		color: light-dark(hsl(var(--h) 62% 38%), hsl(var(--h) 85% 80%));
		font-size: 0.6875rem;
		font-weight: 700;
		letter-spacing: 0.02em;
		line-height: 1;
	}
	.avatar.sm {
		width: 1.25rem;
		height: 1.25rem;
		font-size: 0.5625rem;
	}
</style>
