/** The time, for countdowns, updated every 30 seconds while anything reads it. */
class Clock {
	now = $state(Date.now());
	private timer: ReturnType<typeof setInterval> | undefined;
	private readers = 0;

	/** Keep the clock ticking while a component is on screen: call in an $effect and return what it returns. */
	watch() {
		if (this.readers++ === 0) this.timer = setInterval(() => (this.now = Date.now()), 30_000);
		this.now = Date.now();
		return () => {
			if (--this.readers === 0) clearInterval(this.timer);
		};
	}
}

export const clock = new Clock();
