import test from 'node:test';
import assert from 'node:assert/strict';
import {
  buildBlocksArgs,
  buildCallArgs,
  buildWorkflowArgs,
  filterActions,
  parseOutput,
  toMcpResult,
} from '../src/core.mjs';

test('buildCallArgs pins project/doc and keeps payload structured', () => {
  assert.deepEqual(
    buildCallArgs('pcb.drc.run', {
      project: 'Motor',
      doc: 'PCB1',
      window: 'win-1',
      payload: { rebuild: true },
    }),
    ['--project', 'Motor', '--doc', 'PCB1', 'call', 'pcb.drc.run', '--payload', '{"rebuild":true}', '--window', 'win-1'],
  );
});

test('filterActions applies domain, mutation, and text filters', () => {
  const actions = [
    { name: 'pcb.drc.run', domain: 'pcb', mutates: false, description: 'native check', inputs: [] },
    { name: 'pcb.track.create', domain: 'pcb', mutates: true, description: 'route copper', inputs: ['net'] },
    { name: 'schematic.check', domain: 'schematic', mutates: false, description: 'lint', inputs: [] },
  ];
  assert.deepEqual(filterActions(actions, { domain: 'pcb', mutates: true, search: 'copper' }), [actions[1]]);
});

test('workflow and blocks arguments are shell-free arrays', () => {
  assert.deepEqual(buildWorkflowArgs({ project: 'P', doc: 'PCB1', operation: 'status', reconcile: true }),
    ['--project', 'P', '--doc', 'PCB1', 'workflow', 'status', '--json', '--reconcile']);
  assert.deepEqual(buildWorkflowArgs({ project: 'P', operation: 'confirm', confirmation: 'layout', note: 'reviewed' }),
    ['--project', 'P', 'workflow', 'confirm', 'layout', '--note', 'reviewed']);
  assert.throws(() => buildWorkflowArgs({ project: 'P', operation: 'reset' }), /reset requires/);
  assert.deepEqual(buildBlocksArgs({ operation: 'search', query: 'usb serial' }), ['blocks', 'search', 'usb serial']);
});

test('parseOutput and MCP result preserve structured JSON', () => {
  assert.deepEqual(parseOutput('{"ok":true}'), { ok: true });
  assert.equal(parseOutput('plain'), 'plain');
  const result = toMcpResult({ ok: true, result: { passed: true } });
  assert.deepEqual(result.structuredContent, { passed: true });
  assert.equal(result.isError, false);

  const warned = toMcpResult({ ok: true, result: { passed: true }, stderr: 'staleRisk' });
  assert.deepEqual(warned.structuredContent, { result: { passed: true }, warnings: 'staleRisk' });
});
