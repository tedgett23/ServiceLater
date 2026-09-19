import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		sveltekit({
			compilerOptions: {
				// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
				runes: ({ filename }) =>
					filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},

			// SPA build, embedded into the Go binary (backend/internal/webui/dist).
			adapter: adapter({
				pages: '../backend/internal/webui/dist',
				assets: '../backend/internal/webui/dist',
				fallback: 'index.html',
				precompress: true
			})
		})
	],
	server: {
		proxy: {
			'/api': 'http://localhost:8080',
			'/ws': { target: 'ws://localhost:8080', ws: true }
		}
	}
});
