// Everyone signed in reads the knowledge base; what each person sees is decided by the server.
export const load = async ({ parent }) => {
	const { user } = await parent();
	return { user };
};
