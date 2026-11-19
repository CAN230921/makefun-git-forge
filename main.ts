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
const MAX_TARGET_CACHE = 256;
const MAX_CACHED_TARGET_BYTES = 1024;
const JSON_HEADERS = { 'content-type': 'application/json; charset=utf-8' };
const EMPTY_ARGS = Object.freeze({});
const EMPTY_BODY = Buffer.alloc(0);
type Target = {
  url: URL;
  rest?: string[];
  git?: { owner: string; name: string; route: string };
  malformedPath?: boolean;
  endpoint: 'health' | 'graphql' | 'mcp' | 'rest' | 'git' | 'unknown';
};

function authorized(authorization: string | undefined, token: Buffer) {
  if (!authorization) return false;
  const supplied = authorization.startsWith('Bearer ') ? authorization.slice(7) : parseAuthorization(authorization);
  if (!supplied || supplied.length > token.length) return false;
  const provided = Buffer.from(supplied);
  return token.length === provided.length && timingSafeEqual(token, provided);
}

function parseAuthorization(authorization: string): string | undefined {
  const separator = authorization.indexOf(' ');
  if (separator < 0) return;
  const scheme = authorization.slice(0, separator).toLowerCase();
  const value = authorization.slice(separator + 1);
  if (scheme === 'bearer') return value;
  if (scheme === 'basic') {
    const login = Buffer.from(value, 'base64').toString('utf8');
    const split = login.indexOf(':');
    if (split !== -1) return login.slice(split + 1);
  }
}

function readBody(req: IncomingMessage, limit: number): Promise<Buffer> {
  if (Number(req.headers['content-length']) > limit) {
    req.resume();
    throw new ApiError(413, 'Request body is too large');
  }
  return new Promise((resolve, reject) => {
    let first: Buffer | undefined;
    let more: Buffer[] | undefined;
    let size = 0;
    const onData = (chunk: Buffer) => {
      size += chunk.length;
      if (size > limit) {
        req.off('data', onData);
        first = undefined;
        more = undefined;
        reject(new ApiError(413, 'Request body is too large'));
      } else if (!first) {
        first = chunk;
      } else {
        (more ??= [first]).push(chunk);
      }
    };
    req.on('data', onData);
    req.once('error', reject);
    req.once('end', () => {
      req.off('error', reject);
      if (size <= limit) resolve(more ? Buffer.concat(more, size) : first ?? EMPTY_BODY);
    });
  });
}

function json(res: ServerResponse, reply: Reply) {
  const response = reply.encoded ?? (reply.value === undefined ? undefined : JSON.stringify(reply.value));
  const headers: Record<string, string | number> = { ...JSON_HEADERS, ...reply.headers };
  if (response !== undefined) {
    headers['content-length'] = Buffer.byteLength(response);
    res.writeHead(reply.status, headers);
    res.end(response);
  } else {
    res.writeHead(reply.status, headers);
    res.end();
  }
}

function parseTarget(origin: string, raw: string): Target {
  const url = new URL(raw, origin);
  const pathname = url.pathname;
  if (pathname === '/healthz') return { url, endpoint: 'health' };
  if (pathname === '/api/graphql') return { url, endpoint: 'graphql' };
  if (pathname === '/mcp') return { url, endpoint: 'mcp' };
  if (pathname === API_PREFIX || pathname.startsWith(API_PREFIX + '/')) {
    try {
      const rest = pathname.slice(API_PREFIX.length + 1).split('/').map(decodeURIComponent);
      return { url, endpoint: 'rest', rest };
    } catch (error) {
      if (!(error instanceof URIError)) throw error;
      return { url, endpoint: 'rest', malformedPath: true };
    }
  }
  const match = pathname.match(GIT_ROUTE);
  if (match) {
    try {
      const git = { owner: decodeURIComponent(match[1]), name: decodeURIComponent(match[2]), route: match[3] };
      return { url, endpoint: 'git', git };
    } catch (error) {
      if (!(error instanceof URIError)) throw error;
      return { url, endpoint: 'git', malformedPath: true };
    }
  }
  return { url, endpoint: 'unknown' };
}

export function createForgeServer(options: { token: string; owner?: string }) {
  if (!options.token) throw new Error('MAKEFUN_SHOP_TOKEN or FORGE_TOKEN must be configured');
  const token = Buffer.from(options.token);
  const forge = new Forge(options.owner);
  const targets = new Map<string, Target>();
  let requestIndex = 0;

  function getTarget(req: IncomingMessage) {
    const host = req.headers.host ?? 'localhost';
    const raw = req.url ?? '/';
    if (host.length + raw.length > MAX_CACHED_TARGET_BYTES) return parseTarget('http://' + host, raw);
    const key = host + '\0' + raw;
    let target = targets.get(key);
    if (!target) {
      target = parseTarget('http://' + host, raw);
      if (targets.size >= MAX_TARGET_CACHE) targets.delete(targets.keys().next().value!);
      targets.set(key, target);
    }
    return target;
  }

  async function serve(req: IncomingMessage, res: ServerResponse) {
    const target = getTarget(req);
    const { endpoint, url, git } = target;
    const method = req.method ?? 'GET';
    const isRest = endpoint === 'rest';
    const isGraphQL = endpoint === 'graphql';
    const isMcp = endpoint === 'mcp';
    if (endpoint === 'health' && method === 'GET') return json(res, { status: 200, value: { ok: true } });
    if (endpoint === 'unknown' || endpoint === 'health') return missing();
    if (isRest) res.setHeader('x-github-request-id', (++requestIndex).toString(16).padStart(8, '0'));
    if (!authorized(req.headers.authorization, token)) {
      return json(res, {
        status: 401, value: { message: 'Bad credentials' },
        headers: endpoint === 'git' ? { 'www-authenticate': 'Basic realm="Forge"' } : undefined,
      });
    }
    if (target.malformedPath) throw new ApiError(400, 'Malformed URL');
    if (git) {
      const advertise = git.route === 'info/refs';
      const service = advertise ? url.searchParams.get('service') : git.route;
      if ((service !== 'git-upload-pack' && service !== 'git-receive-pack') || method !== (advertise ? 'GET' : 'POST')) return missing();
      const repo = forge.getRepo(git.owner, git.name);
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
    let parsed: unknown = EMPTY_ARGS;
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
    return json(res, rest(forge, method, url, target.rest!, record(parsed)));
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
