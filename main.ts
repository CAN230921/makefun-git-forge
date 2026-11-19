import { timingSafeEqual } from 'node:crypto';
import { once } from 'node:events';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { setTimeout as sleep } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';
import { graphql } from './graphql.ts';
import { mcp, record, rpcError } from './mcp.ts';
import { rest, type Reply } from './rest.ts';
import { ApiError, Forge, missing } from './store.ts';

const MAX_JSON = 4 * 1024 * 1024;
const MAX_PACK = 64 * 1024 * 1024;
const API_PREFIX = '/api/v3';
const GIT_ROUTE = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/;

function authorized(authorization: string | undefined, token: Buffer) {
  if (!authorization) return false;
  const separator = authorization.indexOf(' ');
  if (separator < 0) return false;
  const scheme = authorization.slice(0, separator).toLowerCase();
  const value = authorization.slice(separator + 1);
  let credential = '';
  if (scheme === 'bearer') {
    credential = value;
  } else if (scheme === 'basic') {
    const login = Buffer.from(value, 'base64').toString('utf8');
    const split = login.indexOf(':');
    credential = split === -1 ? '' : login.slice(split + 1);
  }
  const provided = Buffer.from(credential);
  return token.length === provided.length && timingSafeEqual(token, provided);
}

async function readBody(req: IncomingMessage, limit: number): Promise<Buffer> {
  if (Number(req.headers['content-length']) > limit) {
    req.resume();
    throw new ApiError(413, 'Request body is too large');
  }
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req.iterator({ destroyOnReturn: false })) {
    size += chunk.length;
    if (size > limit) {
      req.resume();
      throw new ApiError(413, 'Request body is too large');
    }
    chunks.push(chunk);
  }
  return chunks.length === 1 ? chunks[0] : Buffer.concat(chunks, size);
}

function json(res: ServerResponse, reply: Reply) {
  res.statusCode = reply.status;
  res.setHeader('content-type', 'application/json; charset=utf-8');
  for (const [name, value] of Object.entries(reply.headers ?? {})) res.setHeader(name, value);
  if (reply.value !== undefined) {
    const response = JSON.stringify(reply.value);
    res.setHeader('content-length', Buffer.byteLength(response));
    res.end(response);
  } else {
    res.end();
  }
}

export function createForgeServer(options: { token: string; owner?: string }) {
  if (!options.token) throw new Error('MAKEFUN_SHOP_TOKEN or FORGE_TOKEN must be configured');
  const token = Buffer.from(options.token);
  const forge = new Forge(options.owner);
  let requestIndex = 0;
  async function serve(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url ?? '/', 'http://' + (req.headers.host ?? 'localhost'));
    const pathname = url.pathname;
    const method = req.method ?? 'GET';
    const isRest = pathname === API_PREFIX || pathname.startsWith(API_PREFIX + '/');
    const isGraphQL = pathname === '/api/graphql';
    const isMcp = pathname === '/mcp';
    const git = pathname.match(GIT_ROUTE);
    if (pathname === '/healthz' && method === 'GET') return json(res, { status: 200, value: { ok: true } });
    if (!git && !isRest && !isGraphQL && !isMcp) return missing();
    if (isRest) res.setHeader('x-github-request-id', (++requestIndex).toString(16).padStart(8, '0'));
    if (!authorized(req.headers.authorization, token)) {
      return json(res, {
        status: 401, value: { message: 'Bad credentials' },
        headers: git ? { 'www-authenticate': 'Basic realm="Forge"' } : undefined,
      });
    }
    if (git) {
      const service = git[3] === 'info/refs' ? url.searchParams.get('service') : git[3];
      const advertise = git[3] === 'info/refs';
      if ((service !== 'git-upload-pack' && service !== 'git-receive-pack') || method !== (advertise ? 'GET' : 'POST')) return missing();
      const repo = forge.getRepo(decodeURIComponent(git[1]), decodeURIComponent(git[2]));
      const v2 = /(?:^|:)version=2(?:$|:)/.test(String(req.headers['git-protocol'] ?? ''));
      let body: Buffer;
      try {
        if (advertise) body = repo.advertise(service, v2);
        else {
          const input = await readBody(req, MAX_PACK);
          body = service === 'git-receive-pack' ? repo.receive(input) : repo.upload(input, v2);
        }
      } catch (error) {
        if (error instanceof ApiError) throw error;
        throw new ApiError(400, 'Malformed Git request');
      }
      res.writeHead(200, {
        'content-type': 'application/x-' + service + (advertise ? '-advertisement' : '-result'),
        'cache-control': 'no-cache',
        'content-length': body.length,
      });
      res.end(body);
      return;
    }
    if (isMcp && method === 'DELETE') return json(res, { status: 204 });
    if ((isMcp || isGraphQL) && method !== 'POST') return json(res, { status: 405, value: { message: 'Method Not Allowed' } });
    let parsed: unknown = {};
    if (method !== 'GET' && method !== 'HEAD') {
      const body = await readBody(req, MAX_JSON);
      if (body.length || !isRest) {
        try {
          parsed = JSON.parse(body.toString('utf8'));
        } catch {
          if (isMcp) return json(res, rpcError(-32700, 'Parse error', null, 400));
          throw new ApiError(400, 'Malformed JSON');
        }
      }
    }
    if (isGraphQL) return json(res, { status: 200, value: graphql(forge, parsed) });
    if (isMcp) return json(res, mcp(forge, record(parsed)));
    return json(res, rest(forge, method, url, record(parsed)));
  }
  return createServer((req, res) => {
    void serve(req, res).catch((error: unknown) => {
      if (res.headersSent) { res.destroy(); return; }
      const malformed = error instanceof URIError || (error instanceof TypeError && 'code' in error && error.code === 'ERR_INVALID_URL');
      const status = error instanceof ApiError ? error.status : malformed ? 400 : 500;
      const message = error instanceof ApiError ? error.message : status === 400 ? 'Malformed URL' : 'Internal Server Error';
      json(res, { status, value: { message } });
    });
  });
}

async function register(origin: string, port: number) {
  for (let attempt = 0; attempt < 8; attempt++) {
    const result = await fetch(origin + '/register', {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ port }),
      signal: AbortSignal.timeout(1500), redirect: 'error',
    }).catch(() => undefined);
    if (result) {
      await result.arrayBuffer();
      if (result.ok) return;
      if ([400, 405, 409].includes(result.status)) throw new Error('Trial registration failed with HTTP ' + result.status);
    }
    await sleep(Math.min(100 * 2 ** attempt, 800));
  }
  throw new Error('Trial registration timed out');
}

async function waitForDone(origin: string) {
  for (;;) {
    const ready = await fetch(origin + '/done', { signal: AbortSignal.timeout(3000), redirect: 'error' })
      .then(async (response) => response.ok ? (await response.json()).done === true : false)
      .catch(() => false);
    if (ready) return;
    await sleep(125);
  }
}

async function main() {
  const environment = process.env.MAKEFUN_ENV_URL?.replace(/\/+$/, '');
  const owner = process.env.FORGE_OWNER ?? (environment ? undefined : 'forge');
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? process.env.FORGE_TOKEN ?? '';
  const server = createForgeServer({ token, owner });
  const port = environment ? 0 : Number(process.env.FORGE_PORT ?? 0);
  const host = environment ? '0.0.0.0' : process.env.FORGE_HOST ?? '127.0.0.1';
  server.listen(port, host);
  await once(server, 'listening');
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Forge did not bind a TCP port');
  console.log('[forge] listening on port ' + address.port);
  if (!environment) return;
  try {
    await register(environment, address.port);
    await waitForDone(environment);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}
