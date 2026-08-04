import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

export const DOMAIN_NAMES = [
  'artifact',
  'board',
  'document',
  'pcb',
  'project',
  'schematic',
  'system',
];

export function easyedaBinary() {
  return process.env.EASYEDA_BIN || 'easyeda';
}

export async function runEasyeda(args, timeoutMs = 300_000) {
  try {
    const { stdout, stderr } = await execFileAsync(easyedaBinary(), args, {
      encoding: 'utf8',
      maxBuffer: 32 * 1024 * 1024,
      timeout: timeoutMs,
    });
    return {
      ok: true,
      result: parseOutput(stdout),
      stderr: stderr.trim() || undefined,
    };
  }
  catch (error) {
    return {
      ok: false,
      error: {
        message: error.message,
        code: error.code ?? null,
        stdout: parseOutput(error.stdout || ''),
        stderr: String(error.stderr || '').trim() || undefined,
      },
    };
  }
}

export function parseOutput(stdout) {
  const text = String(stdout).trim();
  if (!text) return null;
  try {
    return JSON.parse(text);
  }
  catch {
    return text;
  }
}

export function filterActions(actions, { domain, search, mutates } = {}) {
  const needle = String(search || '').trim().toLowerCase();
  return actions.filter((action) => {
    if (domain && action.domain !== domain) return false;
    if (typeof mutates === 'boolean' && action.mutates !== mutates) return false;
    if (!needle) return true;
    return [action.name, action.description, ...(action.inputs || [])]
      .join(' ')
      .toLowerCase()
      .includes(needle);
  });
}

export function buildCallArgs(action, input = {}) {
  const args = [];
  if (input.project) args.push('--project', input.project);
  if (input.doc) args.push('--doc', input.doc);
  args.push('call', action);
  if (input.payload && Object.keys(input.payload).length > 0) {
    args.push('--payload', JSON.stringify(input.payload));
  }
  if (input.window) args.push('--window', input.window);
  return args;
}

export function buildWorkflowArgs(input) {
  if (!input.project) throw new Error('project is required for workflow operations');
  const args = ['--project', input.project];
  if (input.doc) args.push('--doc', input.doc);
  args.push('workflow', input.operation);
  switch (input.operation) {
    case 'init':
      break;
    case 'status':
      args.push('--json');
      if (input.reconcile) args.push('--reconcile');
      break;
    case 'advance':
      if (Number.isInteger(input.minScore)) args.push('--min-score', String(input.minScore));
      if (Number.isInteger(input.maxCrossings)) args.push('--max-crossings', String(input.maxCrossings));
      break;
    case 'confirm':
      if (!['layout', 'outline'].includes(input.confirmation)) {
        throw new Error('confirmation must be layout or outline');
      }
      args.push(input.confirmation);
      if (input.note) args.push('--note', input.note);
      break;
    case 'reset':
      if (input.resetAll === true) args.push('--all');
      else if (input.resetFrom) args.push('--from', input.resetFrom);
      else throw new Error('reset requires resetAll=true or resetFrom');
      break;
    default:
      throw new Error(`unsupported workflow operation: ${input.operation}`);
  }
  return args;
}

export function buildBlocksArgs(input) {
  switch (input.operation) {
    case 'list':
      return ['blocks', 'ls'];
    case 'search':
      if (!input.query) throw new Error('query is required for blocks search');
      return ['blocks', 'search', input.query];
    case 'show':
      if (!input.id) throw new Error('id is required for blocks show');
      return ['blocks', 'show', input.id];
    default:
      throw new Error(`unsupported blocks operation: ${input.operation}`);
  }
}

export function toMcpResult(execution) {
  const rawValue = execution.ok ? execution.result : execution.error;
  const value = execution.ok && execution.stderr
    ? { result: rawValue, warnings: execution.stderr }
    : rawValue;
  const result = {
    content: [{ type: 'text', text: JSON.stringify(value, null, 2) }],
    isError: !execution.ok,
  };
  if (execution.ok && value && typeof value === 'object' && !Array.isArray(value)) {
    result.structuredContent = value;
  }
  return result;
}
