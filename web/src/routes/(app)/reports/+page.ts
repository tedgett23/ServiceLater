import { redirect } from '@sveltejs/kit';

// The SLA report is the only report so far.
export const load = () => redirect(307, '/reports/slas');
