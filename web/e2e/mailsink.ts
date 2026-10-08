import net from 'node:net';

/** One email the sink accepted. */
export type Mail = { from: string; to: string[]; data: string };

/**
 * A mail catcher for the e2e run: a minimal SMTP server on 127.0.0.1 that accepts everything and keeps it in memory.
 * The servicelater worker sends to it with encryption "none", which the server allows only to this machine.
 */
export async function mailSink() {
	const messages: Mail[] = [];
	const server = net.createServer((sock) => {
		let buf = '';
		let inData = false;
		let cur: Mail = { from: '', to: [], data: '' };
		const reply = (s: string) => sock.write(s + '\r\n');
		const handle = (line: string) => {
			if (inData) {
				if (line === '.') {
					inData = false;
					messages.push(cur);
					cur = { from: '', to: [], data: '' };
					reply('250 OK');
				} else {
					cur.data += (line.startsWith('..') ? line.slice(1) : line) + '\n';
				}
				return;
			}
			const verb = line.split(/[ :]/)[0].toUpperCase();
			const addr = () => line.match(/<([^>]*)>/)?.[1] ?? '';
			switch (verb) {
				case 'EHLO':
				case 'HELO':
					return reply('250 mailsink');
				case 'MAIL':
					cur.from = addr();
					return reply('250 OK');
				case 'RCPT':
					cur.to.push(addr());
					return reply('250 OK');
				case 'DATA':
					inData = true;
					return reply('354 End data with <CR><LF>.<CR><LF>');
				case 'RSET':
					cur = { from: '', to: [], data: '' };
					return reply('250 OK');
				case 'NOOP':
					return reply('250 OK');
				case 'QUIT':
					reply('221 Bye');
					return sock.end();
				default:
					return reply('502 Command not implemented');
			}
		};
		reply('220 mailsink ESMTP');
		sock.on('data', (chunk) => {
			buf += chunk.toString('utf8');
			for (let i = buf.indexOf('\r\n'); i >= 0; i = buf.indexOf('\r\n')) {
				const line = buf.slice(0, i);
				buf = buf.slice(i + 2);
				handle(line);
			}
		});
		sock.on('error', () => {});
	});
	await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
	const port = (server.address() as net.AddressInfo).port;
	return {
		port,
		messages,
		/** Waits up to ten seconds for an email matching `match`. */
		async waitFor(match: (m: Mail) => boolean): Promise<Mail> {
			for (let waited = 0; waited < 10_000; waited += 100) {
				const m = messages.find(match);
				if (m) return m;
				await new Promise((r) => setTimeout(r, 100));
			}
			throw new Error(`no matching email; got ${messages.length}: ${messages.map((m) => m.to.join(',')).join('; ')}`);
		},
		close: () => new Promise<void>((resolve) => server.close(() => resolve()))
	};
}
