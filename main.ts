import { fileURLToPath } from 'node:url';

const entry = fileURLToPath(new URL('./forge-go', import.meta.url));
if (process.platform !== 'linux' || process.arch !== 'x64' || !process.execve) {
  throw new Error('This entry requires Linux amd64 and Node 22 with process.execve');
}
process.execve(entry, [entry], process.env as Record<string, string>);
