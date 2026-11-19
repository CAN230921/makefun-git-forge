import { GitRepository, validRef } from './git.ts';
import { isOid, type TreeEntry } from './objects.ts';

export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export const missing = (): never => { throw new ApiError(404, 'Not Found'); };
export const invalid = (message: string): never => { throw new ApiError(422, message); };
const ownerPattern = /^[a-z0-9][a-z0-9-]{0,38}$/i;
const repoPattern = /^(?!\.{1,2}$)[a-z0-9_.-]{1,100}$/i;
const branchRef = (branch: string) => 'refs/heads/' + branch;
export const globalId = (type: string, id: number) => Buffer.from(type + ':' + id).toString('base64url');
export const text = (input: unknown) => typeof input === 'string' ? input : '';
const requiredText = (input: unknown, field: string) => text(input).trim() ? text(input) : invalid(field + ' is required');

type RecordInput = Record<string, unknown>;
type File = { mode: string; oid: string };
type Label = { name: string; color: string; description: string };
type Comment = { id: number; node_id: string; body: string; created_at: string; user: { login: string } };
export type Issue = {
  id: number; node_id: string; number: number; title: string; body: string; state: 'open' | 'closed';
  labels: Label[]; comments: number; user: { login: string }; created_at: string;
  updated_at: string; closed_at: string | null;
};
export type Pull = {
  id: number; node_id: string; number: number; title: string; body: string;
  state: 'open' | 'closed'; merged: boolean; merged_at: string | null; merge_commit_sha: string | null;
  head: { ref: string; sha: string }; base: { ref: string; sha: string }; created_at: string;
};

export class Repository extends GitRepository {
  id: number;
  name: string;
  private: boolean;
  description: string;
  labels = new Map<string, Label>();
  issues = new Map<number, Issue>();
  pulls = new Map<number, Pull>();
  comments = new Map<number, Comment[]>();
  nextNumber = 1;

  constructor(id: number, name: string, options: RecordInput) {
    super(text(options.default_branch) || 'main');
    if (!validRef(this.head)) invalid('Invalid default branch');
    this.id = id;
    this.name = name;
    this.private = options.private === true;
    this.description = text(options.description);
  }

  filesAt(oid?: string): Map<string, File> {
    const files = new Map<string, File>();
    const commit = oid ? this.store.commit(this.peel(oid)) : undefined;
    if (!commit) return files;
    const pending = [{ prefix: '', tree: commit.tree }];
    while (pending.length) {
      const { prefix, tree } = pending.pop()!;
      for (const entry of this.store.tree(tree) ?? []) {
        const path = prefix + entry.name;
        if (Number(entry.mode) === 40000) pending.push({ prefix: path + '/', tree: entry.oid });
        else files.set(path, { mode: entry.mode, oid: entry.oid });
      }
    }
    return files;
  }

  writeTree(files: Map<string, File>): string {
    type Directory = Map<string, File | Directory>;
    const root: Directory = new Map();
    for (const [path, file] of files) {
      const parts = path.split('/');
      const leaf = parts.pop()!;
      let parent = root;
      for (const part of parts) {
        let child = parent.get(part);
        if (!child) {
          child = new Map();
          parent.set(part, child);
        }
        if (!(child instanceof Map)) invalid('File and directory share a path');
        parent = child as Directory;
      }
      if (parent.has(leaf)) invalid('File and directory share a path');
      parent.set(leaf, file);
    }
    const write = (dir: Directory): string => {
      const entries: TreeEntry[] = [...dir].map(([name, child]) => child instanceof Map
        ? { name, mode: '40000', oid: write(child) } : { name, ...child });
      const orderKey = (entry: TreeEntry) => Buffer.from(entry.name + (entry.mode === '40000' ? '/' : ''));
      entries.sort((a, b) => Buffer.compare(orderKey(a), orderKey(b)));
      const chunks = entries.flatMap(({ mode, name, oid }) => [Buffer.from(mode + ' ' + name + '\0'), Buffer.from(oid, 'hex')]);
      return this.store.put('tree', Buffer.concat(chunks));
    };
    return write(root);
  }

  writeCommit(files: Map<string, File>, branch: string, parent: string | undefined, message: string, timestamp: number): string {
    const tree = this.writeTree(files);
    const identity = 'Forge <forge@localhost> ' + timestamp + ' +0000';
    const contents = 'tree ' + tree + '\n' + (parent ? 'parent ' + parent + '\n' : '')
      + 'author ' + identity + '\ncommitter ' + identity + '\n\n' + message + '\n';
    const oid = this.store.put('commit', Buffer.from(contents));
    this.refs.set(branchRef(branch), oid);
    return oid;
  }

  filesForPull(pull: Pull) {
    const { from, incoming } = this.pullTrees(pull);
    const result: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
    for (const path of [...new Set([...from.keys(), ...incoming.keys()])].sort()) {
      const old = from.get(path);
      const next = incoming.get(path);
      if (sameFile(old, next)) continue;
      const [additions, deletions] = lineChanges(
        old ? this.store.objects.get(old.oid)?.bytes : undefined,
        next ? this.store.objects.get(next.oid)?.bytes : undefined,
      );
      result.push({ filename: path, status: !old ? 'added' : !next ? 'removed' : 'modified', additions, deletions, changes: additions + deletions });
    }
    return result;
  }

  pullTrees(pull: Pull) {
    const base = pull.merged ? pull.base.sha : this.refs.get(branchRef(pull.base.ref)) ?? pull.base.sha;
    const head = pull.merged ? pull.head.sha : this.refs.get(branchRef(pull.head.ref)) ?? pull.head.sha;
    return { base, head, from: this.filesAt(this.mergeBase(base, head)), target: this.filesAt(base), incoming: this.filesAt(head) };
  }

  mergedFiles(pull: Pull): Map<string, File> | null {
    const { from, target, incoming } = this.pullTrees(pull);
    const merged = new Map(target);
    for (const path of new Set([...from.keys(), ...incoming.keys()])) {
      const previous = from.get(path);
      const changed = incoming.get(path);
      const current = target.get(path);
      if (sameFile(previous, changed)) continue;
      if (!sameFile(previous, current) && !sameFile(changed, current)) return null;
      if (changed) merged.set(path, changed);
      else merged.delete(path);
    }
    for (const path of merged.keys()) {
      let index = path.indexOf('/');
      while (index !== -1) {
        if (merged.has(path.slice(0, index))) return null;
        index = path.indexOf('/', index + 1);
      }
    }
    return merged;
  }
}

const sameFile = (a?: File, b?: File) => a?.oid === b?.oid && a?.mode === b?.mode;

function lineChanges(before?: Buffer, after?: Buffer): [number, number] {
  if (before?.includes(0) || after?.includes(0)) return [0, 0];
  const lines = (bytes?: Buffer) => bytes?.length ? bytes.toString('utf8').match(/[^\n]*\n|[^\n]+$/g) ?? [] : [];
  const a = lines(before);
  const b = lines(after);
  let start = 0;
  let aEnd = a.length;
  let bEnd = b.length;
  while (start < aEnd && start < bEnd && a[start] === b[start]) start++;
  while (aEnd > start && bEnd > start && a[aEnd - 1] === b[bEnd - 1]) { aEnd--; bEnd--; }
  const n = aEnd - start;
  const m = bEnd - start;
  if (n * m > 300000) return [m, n];
  const length = new Uint32Array(m + 1);
  for (let i = 0; i < n; i++) {
    let previous = 0;
    for (let j = 1; j <= m; j++) {
      const left = length[j];
      length[j] = a[start + i] === b[start + j - 1] ? previous + 1 : Math.max(left, length[j - 1]);
      previous = left;
    }
  }
  return [m - length[m], n - length[m]];
}

export class Forge {
  private ownerName?: string;
  private nextId = 1;
  private second = 1735689600;
  repos = new Map<string, Repository>();
  nodes = new Map<string, { type: string; repo: Repository; number?: number }>();

  constructor(owner?: string) {
    if (owner && !ownerPattern.test(owner)) throw new Error('Invalid FORGE_OWNER');
    this.ownerName = owner;
  }

  get owner() { return this.ownerName ?? 'forge'; }

  timestamp() { return this.second++; }

  date(second = this.timestamp()) { return new Date(second * 1000).toISOString(); }

  allocate() { return this.nextId++; }

  getRepo(owner: string, name: string): Repository {
    const repo = this.repos.get(name.toLowerCase());
    if (!repo || !ownerPattern.test(owner) || (this.ownerName && this.ownerName.toLowerCase() !== owner.toLowerCase())) return missing();
    // The trial provides one trusted bearer token but no explicit login. Bind that token's
    // namespace on its first lookup of an already-created repository.
    this.ownerName ??= owner;
    return repo;
  }

  createRepo(input: RecordInput): Repository {
    const name = requiredText(input.name, 'name');
    if (!repoPattern.test(name) || this.repos.has(name.toLowerCase())) return invalid('Repository name is invalid or already exists');
    const repo = new Repository(this.allocate(), name, input);
    this.repos.set(name.toLowerCase(), repo);
    this.nodes.set(globalId('Repository', repo.id), { type: 'Repository', repo });
    if (input.auto_init === true) {
      const oid = repo.store.put('blob', Buffer.from('# ' + name + '\n'));
      repo.writeCommit(new Map([['README.md', { oid, mode: '100644' }]]), repo.defaultBranch, undefined, 'Initial commit', this.timestamp());
    }
    return repo;
  }

  deleteRepo(repo: Repository) {
    this.repos.delete(repo.name.toLowerCase());
    for (const [id, node] of this.nodes) if (node.repo === repo) this.nodes.delete(id);
  }

  repoJson(repo: Repository) {
    return {
      id: repo.id, node_id: globalId('Repository', repo.id), name: repo.name,
      full_name: this.owner + '/' + repo.name, owner: { login: this.owner },
      private: repo.private, description: repo.description, default_branch: repo.defaultBranch, has_issues: true,
    };
  }

  createLabel(repo: Repository, input: RecordInput): Label {
    const name = requiredText(input.name, 'name');
    const key = name.toLowerCase();
    if (repo.labels.has(key)) return invalid('Label already exists');
    const label = { name, color: text(input.color) || 'ededed', description: text(input.description) };
    repo.labels.set(key, label);
    return label;
  }

  createIssue(repo: Repository, input: RecordInput): Issue {
    const title = requiredText(input.title, 'title');
    const requested = Array.isArray(input.labels) ? input.labels : [];
    const labels = requested.map((name) => repo.labels.get(text(name).toLowerCase()) ?? invalid('Unknown issue label'));
    const id = this.allocate();
    const date = this.date();
    const issue: Issue = {
      id, node_id: globalId('Issue', id), number: repo.nextNumber++, title, body: text(input.body), state: 'open',
      labels, comments: 0, user: { login: this.owner }, created_at: date, updated_at: date, closed_at: null,
    };
    repo.issues.set(issue.number, issue);
    this.nodes.set(issue.node_id, { type: 'Issue', repo, number: issue.number });
    return issue;
  }

  getIssue(repo: Repository, number: number) { return repo.issues.get(number) ?? missing(); }

  updateIssue(issue: Issue, input: RecordInput) {
    if (input.state !== undefined && input.state !== 'open' && input.state !== 'closed') invalid('Invalid issue state');
    if (input.title !== undefined) issue.title = requiredText(input.title, 'title');
    if (input.body !== undefined) issue.body = text(input.body);
    const date = this.date();
    if (input.state === 'open' || input.state === 'closed') {
      if (issue.state !== input.state) issue.closed_at = input.state === 'closed' ? date : null;
      issue.state = input.state;
    }
    issue.updated_at = date;
    return issue;
  }

  issues(repo: Repository, state: string, ascending = false) {
    const issues = [...repo.issues.values()].filter((issue) => state === 'all' || issue.state === state);
    return ascending ? issues : issues.reverse();
  }

  addComment(repo: Repository, number: number, input: RecordInput): Comment {
    const issue = this.getIssue(repo, number);
    const body = requiredText(input.body, 'body');
    const id = this.allocate();
    const comment = { id, node_id: globalId('IssueComment', id), body, created_at: this.date(), user: { login: this.owner } };
    const list = repo.comments.get(number) ?? [];
    list.push(comment);
    repo.comments.set(number, list);
    issue.comments = list.length;
    issue.updated_at = comment.created_at;
    return comment;
  }

  createPull(repo: Repository, input: RecordInput): Pull {
    const title = requiredText(input.title, 'title');
    const head = requiredText(input.head, 'head').split(':').at(-1)!;
    const base = requiredText(input.base, 'base');
    const headSha = repo.refs.get(branchRef(head));
    const baseSha = repo.refs.get(branchRef(base));
    if (!headSha || !baseSha || head === base) return invalid('Invalid head or base branch');
    const id = this.allocate();
    const pull: Pull = {
      id, node_id: globalId('PullRequest', id), number: repo.nextNumber++, title, body: text(input.body), state: 'open',
      merged: false, merged_at: null, merge_commit_sha: null,
      head: { ref: head, sha: headSha }, base: { ref: base, sha: baseSha }, created_at: this.date(),
    };
    repo.pulls.set(pull.number, pull);
    this.nodes.set(pull.node_id, { type: 'PullRequest', repo, number: pull.number });
    return pull;
  }

  getPull(repo: Repository, number: number) { return repo.pulls.get(number) ?? missing(); }

  mergePull(repo: Repository, pull: Pull, input: RecordInput = {}) {
    if (pull.state !== 'open' || !repo.refs.has(branchRef(pull.head.ref)) || !repo.refs.has(branchRef(pull.base.ref))) {
      throw new ApiError(405, 'Pull request cannot be merged');
    }
    const files = repo.mergedFiles(pull);
    if (!files) throw new ApiError(405, 'Pull request has conflicts');
    const { base, head } = repo.pullTrees(pull);
    if (input.sha && input.sha !== head) throw new ApiError(409, 'Head SHA changed');
    const timestamp = this.timestamp();
    const message = text(input.commit_title) || pull.title + ' (#' + pull.number + ')';
    const sha = repo.writeCommit(files, pull.base.ref, base, message, timestamp);
    pull.head.sha = head;
    pull.base.sha = base;
    pull.state = 'closed';
    pull.merged = true;
    pull.merged_at = this.date(timestamp);
    pull.merge_commit_sha = sha;
    return { sha, merged: true, message: 'Pull Request successfully merged' };
  }

  updateFile(repo: Repository, path: string, input: RecordInput, remove: boolean) {
    if (!path || path.length > 4096 || path.split('/').some((part) => !part || part === '.' || part === '..' || part.includes('\0'))) return invalid('Invalid file path');
    const branch = text(input.branch) || repo.defaultBranch;
    if (!validRef(branchRef(branch))) return invalid('Invalid branch');
    const parent = repo.refs.get(branchRef(branch));
    if (!parent && branch !== repo.defaultBranch) return missing();
    const files = repo.filesAt(parent);
    const old = files.get(path);
    if (!old && remove) return missing();
    if ((old && input.sha !== old.oid) || (!old && input.sha !== undefined)) throw new ApiError(409, 'File SHA does not match');
    const message = requiredText(input.message, 'message');
    if (remove) files.delete(path);
    else {
      if (typeof input.content !== 'string' || !/^[A-Za-z0-9+/\s]*={0,2}$/.test(input.content)) return invalid('Invalid base64 content');
      const data = Buffer.from(input.content, 'base64');
      if (repo.store.bytes + data.length > 192 * 1024 * 1024) return invalid('Object store is full');
      const oid = repo.store.put('blob', data);
      files.set(path, { oid, mode: old?.mode ?? '100644' });
    }
    const sha = repo.writeCommit(files, branch, parent, message, this.timestamp());
    return { existed: !!old, commit: { sha }, content: remove ? null : this.readContents(repo, path, sha) };
  }

  readContents(repo: Repository, path: string, ref?: string): unknown {
    const target = repo.resolve(ref);
    const commit = target ? repo.store.commit(repo.peel(target)) : undefined;
    if (!commit) return missing();
    let oid = commit.tree;
    const segments = path ? path.split('/') : [];
    for (const [index, part] of segments.entries()) {
      const entry = repo.store.tree(oid)?.find((item) => item.name === part);
      if (!entry) return missing();
      if (index === segments.length - 1 && Number(entry.mode) !== 40000) {
        const bytes = repo.store.objects.get(entry.oid)?.bytes;
        if (!bytes) return missing();
        return { type: 'file', name: part, path, sha: entry.oid, size: bytes.length, encoding: 'base64', content: bytes.toString('base64') };
      }
      oid = entry.oid;
    }
    const tree = repo.store.tree(oid) ?? missing();
    return tree.map((entry) => ({
      type: Number(entry.mode) === 40000 ? 'dir' : 'file', name: entry.name,
      path: path ? path + '/' + entry.name : entry.name, sha: entry.oid,
    }));
  }

  createRef(repo: Repository, input: RecordInput) {
    const ref = text(input.ref);
    const sha = text(input.sha);
    if (!validRef(ref) || !isOid(sha) || repo.refs.has(ref) || !repo.store.objects.has(sha)
      || (ref.startsWith('refs/heads/') && !repo.store.commit(sha))) return invalid('Invalid or existing ref');
    repo.refs.set(ref, sha);
    return refJson(ref, sha);
  }
}

export const refJson = (ref: string, sha: string) => ({ ref, object: { sha, type: 'commit' } });
