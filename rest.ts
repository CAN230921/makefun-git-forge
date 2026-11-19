import { missing, refJson, text, type Forge } from './store.ts';

export type Reply = { status: number; value?: unknown; headers?: Record<string, string> };
const ok = (value: unknown): Reply => ({ status: 200, value });
const created = (value: unknown): Reply => ({ status: 201, value });
const noContent: Reply = { status: 204 };

function page<T>(values: T[], url: URL): Reply {
  const size = Math.min(100, Math.max(1, Math.floor(Number(url.searchParams.get('per_page')) || 30)));
  const index = Math.max(1, Math.floor(Number(url.searchParams.get('page')) || 1));
  const end = size * index;
  const reply = ok(values.slice(end - size, end));
  if (end < values.length) {
    const next = new URL(url);
    next.searchParams.set('page', String(index + 1));
    reply.headers = { link: '<' + next.href + '>; rel="next"' };
  }
  return reply;
}

export function rest(forge: Forge, method: string, url: URL, body: Record<string, unknown>): Reply {
  const parts = url.pathname.slice('/api/v3'.length).split('/').slice(1).map(decodeURIComponent);
  const [root, owner, name, resource, index, action] = parts;
  if (!root && method === 'GET') {
    return {
      status: 200,
      value: { current_user_url: url.origin + '/api/v3/user', repository_url: url.origin + '/api/v3/repos/{owner}/{repo}' },
      headers: { 'x-oauth-scopes': 'repo, read:org' },
    };
  }
  if (root === 'meta' && parts.length === 1 && method === 'GET') return ok({ installed_version: '3.0.0' });
  if (root === 'user' && parts.length === 1 && method === 'GET') return ok({ login: forge.owner, id: 1 });
  if (root === 'user' && owner === 'repos' && parts.length === 2) {
    if (method === 'POST') return created(forge.repoJson(forge.createRepo(body)));
    if (method === 'GET') return page([...forge.repos.values()].map((repo) => forge.repoJson(repo)), url);
  }
  if (root !== 'repos' || !owner || !name) return missing();
  const repo = forge.getRepo(owner, name);
  if (parts.length === 3) {
    if (method === 'GET') return ok(forge.repoJson(repo));
    if (method === 'DELETE') { forge.deleteRepo(repo); return noContent; }
    if (method === 'PATCH') {
      if (body.description !== undefined) repo.description = text(body.description);
      if (typeof body.private === 'boolean') repo.private = body.private;
      return ok(forge.repoJson(repo));
    }
  }
  if (resource === 'branches' && method === 'GET') {
    const heads = [...repo.refs].filter(([ref]) => ref.startsWith('refs/heads/'))
      .map(([ref, sha]) => ({ name: ref.slice(11), commit: { sha }, protected: false }));
    if (index) return ok(heads.find((head) => head.name === parts.slice(4).join('/')) ?? missing());
    return page(heads, url);
  }
  if (resource === 'commits' && parts.length <= 5 && method === 'GET') {
    const ref = parts[4] ?? url.searchParams.get('sha') ?? repo.defaultBranch;
    const resolved = repo.resolve(ref);
    if (!resolved && (index || url.searchParams.has('sha'))) return missing();
    const format = (sha: string) => {
      const commit = repo.store.commit(sha) ?? missing();
      return { sha, commit: { message: commit.message, tree: { sha: commit.tree } }, parents: commit.parents.map((oid) => ({ sha: oid })) };
    };
    if (index && resolved) return ok(format(resolved));
    const values: ReturnType<typeof format>[] = [];
    const pending = resolved ? [resolved] : [];
    const seen = new Set<string>();
    for (let i = 0; i < pending.length; i++) {
      const sha = pending[i];
      if (seen.has(sha)) continue;
      seen.add(sha);
      const commit = format(sha);
      values.push(commit);
      for (const parent of commit.parents) pending.push(parent.sha);
    }
    return page(values, url);
  }
  if (resource === 'git' && (index === 'refs' || index === 'ref')) {
    if (!action && method === 'POST') return created(forge.createRef(repo, body));
    if (action && method === 'GET') {
      const ref = 'refs/' + parts.slice(5).join('/').replace(/^refs\//, '');
      return ok(refJson(ref, repo.refs.get(ref) ?? missing()));
    }
  }
  if (resource === 'contents') {
    const path = parts.slice(4).join('/');
    if (method === 'GET') return ok(forge.readContents(repo, path, url.searchParams.get('ref') ?? undefined));
    if (method === 'PUT' || method === 'DELETE') {
      const { existed, ...value } = forge.updateFile(repo, path, body, method === 'DELETE');
      return { status: existed ? 200 : 201, value };
    }
  }
  if (resource === 'labels' && !index) {
    if (method === 'POST') return created(forge.createLabel(repo, body));
    if (method === 'GET') return page([...repo.labels.values()], url);
  }
  if (resource === 'issues') {
    if (!index) {
      if (method === 'POST') return created(forge.createIssue(repo, body));
      if (method === 'GET') return page(forge.issues(repo, url.searchParams.get('state') ?? 'open', url.searchParams.get('direction') === 'asc'), url);
    } else if (/^[1-9][0-9]*$/.test(index)) {
      const issue = forge.getIssue(repo, Number(index));
      if (!action && method === 'GET') return ok(issue);
      if (!action && method === 'PATCH') return ok(forge.updateIssue(issue, body));
      if (action === 'comments' && parts.length === 6) {
        if (method === 'POST') return created(forge.addComment(repo, issue.number, body));
        if (method === 'GET') return page(repo.comments.get(issue.number) ?? [], url);
      }
    }
  }
  if (resource === 'pulls') {
    if (!index) {
      if (method === 'POST') return created(forge.createPull(repo, body));
      if (method === 'GET') {
        const state = url.searchParams.get('state') ?? 'open';
        return page([...repo.pulls.values()].reverse().filter((pull) => state === 'all' || pull.state === state), url);
      }
    } else if (/^[1-9][0-9]*$/.test(index)) {
      const pull = forge.getPull(repo, Number(index));
      if (!action && method === 'GET') return ok(pull);
      if (action === 'files' && method === 'GET') return page(repo.filesForPull(pull), url);
      if (action === 'merge' && method === 'PUT') return ok(forge.mergePull(repo, pull, body));
    }
  }
  return missing();
}
