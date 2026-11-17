import { as, expect, test } from './helpers';

const background = (page: import('@playwright/test').Page) =>
	page.locator('body').evaluate((el) => getComputedStyle(el).backgroundColor);

test('a person picks light, dark or automatic on their account, and it follows them to another browser', async ({ browser }) => {
	// Starts on a light device, so choosing dark visibly changes the page.
	const first = await as(browser, 'marcus', { colorScheme: 'light' });
	try {
		await first.page.goto('/account');
		const look = first.page.getByRole('radiogroup', { name: 'Appearance' });
		await expect(look.getByRole('radio', { name: /Automatic/ })).toBeChecked();
		const light = await background(first.page);

		await look.getByText('Dark', { exact: true }).click();
		await expect(look.getByRole('radio', { name: /Dark/ })).toBeChecked();
		await expect(first.page.locator('html')).toHaveAttribute('data-theme', 'dark');
		expect(await background(first.page)).not.toBe(light);

		// Another browser, also on a light device, gets it from the account, with nothing stored locally.
		const second = await as(browser, 'marcus', { colorScheme: 'light' });
		await second.page.goto('/tickets');
		await expect(second.page.locator('html')).toHaveAttribute('data-theme', 'dark');
		await second.context.close();

		// Back to automatic: the device decides again.
		await look.getByText('Automatic', { exact: true }).click();
		await expect(first.page.locator('html')).not.toHaveAttribute('data-theme', /./);
		await first.page.reload();
		await expect(look.getByRole('radio', { name: /Automatic/ })).toBeChecked();
		expect(await background(first.page)).toBe(light);
	} finally {
		await first.context.request.patch('/api/v1/account', { data: { theme: 'auto' } });
	}
});
