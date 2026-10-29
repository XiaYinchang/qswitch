import { test, expect, mock } from 'claude-code/testing';
import type { Engine } from 'claude-code/testing';
import type { On } from 'claude-code';

function setup(on: On, initial = 'idle') {
  const clock = mock.clock(on);
  const control = { action: 'run', generation: '', draft: '', posted: 0, agents: [] as { id: string; description: string; type: string; status: 'running' | 'completed' }[] };
  mock.env(on, {
    QSWITCH_CONTROL_URL: 'http://127.0.0.1:43210',
    QSWITCH_CONTROL_TOKEN: 'test-only-token',
    QSWITCH_INITIAL_CONTINUATION: initial,
  });
  on('session.start', ($, e) => ({ cwd: e.cwd }));
  on('session.id', () => ({ value: 'test-session' }));
  on('session.cwd', () => ({ value: '/test' }));
  on('agent.list', () => ({ value: control.agents }));
  on('prompt.read', () => ({ value: { text: control.draft, cursor: control.draft.length } }));
  on('prompt.fill', ($, e) => {
    control.draft = e.mode === 'append' ? control.draft + e.text : e.text;
    return { isFilled: true, text: control.draft, cursor: control.draft.length };
  });
  on('command.register', ($, e) => ({ value: { command: e.name } }));
  on('http.fetch', () => {
    control.posted += 1;
    return { value: {
      status: 200, ok: true, headers: {},
      text: JSON.stringify({ action: control.action, generation: control.generation }),
    } };
  });
  on('turn.start', ($, e) => ({ turnId: e.turnId }));
  on('turn.complete', ($, e) => ({ text: e.answer }));
  on('classic.StopFailure', () => ({}));
  on('classic.Stop', () => ({}));
  return { clock, control };
}

async function start($: Engine) {
  await $.session.start({ cwd: '/test', surface: 'terminal', isInteractive: true });
}

async function status($: Engine) {
  const result = await $.command.run({
    command: 'qswitch-status', args: '', origin: { kind: 'composer' },
    presentation: { isFullscreen: false, columns: 80 },
  });
  return JSON.parse(result.text ?? '{}');
}

async function complete($: Engine, turnId: string, reason: 'answer' | 'aborted' | 'error') {
  await $.turn.complete({ turnId, reason, answer: '', durationMs: 1, isAborted: reason === 'aborted' });
}

test('rate limit is pending continuation; successful answer and user interruption clear it', async ($, on) => {
  setup(on);
  await start($);
  expect((await status($)).needs_continue).toBe(false);
  await $.turn.start({ turnId: 'limited', text: 'work' });
  expect((await status($)).ready).toBe(false);
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'limited', 'error');
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
  await $.turn.start({ turnId: 'done', text: 'continue' });
  await complete($, 'done', 'answer');
  expect(await status($)).toMatchObject({ ready: true, needs_continue: false });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await $.turn.start({ turnId: 'cancelled', text: 'continue' });
  await complete($, 'cancelled', 'aborted');
  expect(await status($)).toMatchObject({ ready: true, needs_continue: false });
});

test('adopted continuation is pending only and subagent failure does not replace it', async ($, on) => {
  setup(on, 'continue');
  await start($);
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, queued_submissions: 0 });
  await $.classic.StopFailure({ error: 'authentication_failed', agent_id: 'child' });
  expect((await status($)).needs_continue).toBe(true);
});

test('Bash background task survives rate-limit failure until an authoritative empty Stop', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: {
    stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'bg1',
  } }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'background-work', run_in_background: true });
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect(await status($)).toMatchObject({ ready: false, needs_continue: true });
  await $.classic.Stop({ stop_hook_active: false,
    background_tasks: [{ id: 'bg1', type: 'shell', status: 'running', description: 'work' }], session_crons: [] });
  expect((await status($)).ready).toBe(false);
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [], session_crons: [] });
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
});

test('foreground Bash allows rotation but an unknown tool lifecycle requires Stop evidence', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: { stdout: 'done', stderr: '', interrupted: false } }));
  on('tool.call', { tool: 'mcp__external__start' }, () => ({ result: { accepted: true } }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'foreground-work' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect((await status($)).ready).toBe(true);
  await $.tool.call({ tool: 'mcp__external__start' });
  expect((await status($)).ready).toBe(false);
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect((await status($)).ready).toBe(false);
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [], session_crons: [] });
  expect((await status($)).ready).toBe(true);
});

test('live agents and scheduled tasks block readiness', async ($, on) => {
  const { control } = setup(on);
  await start($);
  control.agents = [{ id: 'child', description: 'work', type: 'general-purpose', status: 'running' }];
  expect((await status($)).ready).toBe(false);
  control.agents[0]!.status = 'completed';
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [],
    session_crons: [{ id: 'cron1', schedule: '* * * * *', recurring: true, prompt: 'work' }] });
  expect((await status($)).ready).toBe(false);
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [], session_crons: [] });
  expect((await status($)).ready).toBe(true);
});

test('prepare drops user submission, restores text, and blocks readiness until release', async ($, on) => {
  const { clock, control } = setup(on);
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  const result = await $.prompt.submit({ text: 'do not lose this', wait: false, origin: { kind: 'composer' } });
  expect(result.drop).toContain('not sent');
  expect(control.draft).toBe('do not lose this');
  expect((await status($)).ready).toBe(false);
  control.action = 'run';
  control.generation = '';
  await clock.advance(750);
  expect((await status($)).ready).toBe(false);
  control.draft = '';
  expect((await status($)).ready).toBe(true);
});

test('accepted queued text blocks rotation until its model turn starts and finishes', async ($, on) => {
  setup(on);
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.prompt.submit({ text: 'queued work', wait: true, origin: { kind: 'composer' } });
  expect(await status($)).toMatchObject({ ready: false, queued_submissions: 1 });
  await $.turn.start({ turnId: 'queued-turn', text: 'queued work' });
  expect(await status($)).toMatchObject({ ready: false, queued_submissions: 0 });
  await complete($, 'queued-turn', 'answer');
  expect((await status($)).ready).toBe(true);
});

test('unknown model failure blocks until its official failure outcome is known', async ($, on) => {
  setup(on);
  await start($);
  await $.turn.start({ turnId: 'unknown-error', text: 'work' });
  await complete($, 'unknown-error', 'error');
  expect((await status($)).ready).toBe(false);
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
});

test('rotation exit keeps its acknowledgement while Claude shuts down', async ($, on) => {
  const { clock, control } = setup(on);
  let exits = 0;
  on('command.run', { command: 'exit' }, () => { exits += 1; return { text: '' }; });
  await start($);
  control.action = 'exit';
  control.generation = 'generation-1';
  await clock.advance(750);
  expect(exits).toBe(1);
  const afterExit = control.posted;
  await clock.advance(10000);
  // Any heartbeat here would revoke the runner's exit acknowledgement.
  expect(control.posted).toBe(afterExit);
  await clock.advance(20000);
  expect(await status($)).toMatchObject({ ready: false, reason: 'exit command returned without terminating the session' });
});

test('a failed foreground Bash call does not leave an uncertain task', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ isError: true, result: 'Error: Exit code 1' }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'false' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, uncertain_tasks: [] });
});

test('a completed task notification retires its background task without a Stop', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: {
    stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'bg1',
  } }));
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'release', run_in_background: true });
  expect((await status($)).ready).toBe(false);
  const text = '<task-notification>\n<task-id>bg1</task-id>\n<status>completed</status>\n</task-notification>';
  await $.prompt.submit({ text, wait: true, origin: { kind: 'task-notification', producer: 'session-task' } });
  await $.turn.start({ turnId: 'notified', text });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'notified', 'error');
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, background_tasks: [] });
});

test('a running task notification keeps its background task', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: {
    stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'bg1',
  } }));
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'watch', run_in_background: true });
  const text = '<task-notification>\n<task-id>bg1</task-id>\n<status>running</status>\n</task-notification>';
  await $.prompt.submit({ text, wait: true, origin: { kind: 'task-notification', producer: 'session-task' } });
  await $.turn.start({ turnId: 'notified', text });
  await complete($, 'notified', 'answer');
  expect(await status($)).toMatchObject({ ready: false, background_tasks: [{ id: 'bg1', type: 'shell' }] });
});
