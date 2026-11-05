import { test, expect, mock } from 'claude-code/testing';
import type { Engine } from 'claude-code/testing';
import type { On } from 'claude-code';

function setup(on: On, initial = 'idle') {
  const clock = mock.clock(on);
  const control = {
    action: 'run', generation: '', draft: '', posted: 0, restore: '',
    agents: [] as { id: string; description: string; type: string; status: 'running' | 'completed' }[],
    requests: [] as { path: string; body: Record<string, unknown> }[],
  };
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
  on('http.fetch', ($, e) => {
    control.posted += 1;
    control.requests.push({ path: e.url.slice(e.url.lastIndexOf('/')), body: JSON.parse(e.init?.body ?? '{}') });
    return { value: {
      status: 200, ok: true, headers: {},
      text: JSON.stringify({ action: control.action, generation: control.generation, ...(control.restore ? { restore: control.restore } : {}) }),
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

test('Bash background task stays visible without blocking rate-limit continuation', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: {
    stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'bg1',
  } }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'background-work', run_in_background: true });
  expect(await status($)).toMatchObject({ ready: false, background_tasks: [{ id: 'bg1', type: 'shell' }] });
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect(await status($)).toMatchObject({
    ready: true, needs_continue: true, background_tasks: [{ id: 'bg1', type: 'shell' }],
  });
  await $.classic.Stop({ stop_hook_active: false,
    background_tasks: [{ id: 'bg1', type: 'shell', status: 'running', description: 'work' }], session_crons: [] });
  expect(await status($)).toMatchObject({ ready: true, background_tasks: [{ id: 'bg1', status: 'running' }] });
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [], session_crons: [] });
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, background_tasks: [] });
});

test('unknown tool lifecycle blocks normal readiness but not rate-limit continuation', async ($, on) => {
  setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: { stdout: 'done', stderr: '', interrupted: false } }));
  on('tool.call', { tool: 'mcp__external__start' }, () => ({ result: { accepted: true } }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'foreground-work' });
  expect((await status($)).ready).toBe(true);
  await $.tool.call({ tool: 'mcp__external__start' });
  expect((await status($)).ready).toBe(false);
  await $.classic.StopFailure({ error: 'rate_limit' });
  const interrupted = await status($);
  expect(interrupted).toMatchObject({ ready: true, needs_continue: true });
  expect(interrupted.uncertain_tasks).toHaveLength(1);
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [], session_crons: [] });
  expect(await status($)).toMatchObject({ ready: true, uncertain_tasks: [] });
});

test('live agents and scheduled tasks block normal readiness but not rate-limit continuation', async ($, on) => {
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
  control.agents[0]!.status = 'running';
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [],
    session_crons: [{ id: 'cron1', schedule: '* * * * *', recurring: true, prompt: 'work' }] });
  await $.classic.StopFailure({ error: 'rate_limit' });
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, session_crons: [{ id: 'cron1' }] });
});

// On 11-04 a switch waited ten minutes for a scheduled task due the next day.
test('a switch does not wait for a scheduled task, but an upgrade does', async ($, on) => {
  const { clock, control } = setup(on);
  await start($);
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [],
    session_crons: [{ id: 'cron1', schedule: '7 10 5 11 *', recurring: false, prompt: 'check' }] });
  expect(await status($)).toMatchObject({ ready: false, reason: 'scheduled session tasks remain' });
  control.action = 'prepare';
  control.generation = 'generation-1';
  await clock.advance(750);
  expect(await status($)).toMatchObject({ ready: true, phase: 'prepare', stopped: ['scheduled task cron1'] });
  control.action = 'run';
  control.generation = '';
  await clock.advance(750);
  expect(await status($)).toMatchObject({ ready: false, reason: 'scheduled session tasks remain' });
});

test('an active foreground tool still blocks rate-limit continuation', async ($, on) => {
  setup(on);
  let releaseTool!: () => void;
  const toolGate = new Promise<void>(resolve => { releaseTool = resolve; });
  on('tool.call', { tool: 'Bash' }, async () => {
    await toolGate;
    return { result: { stdout: 'done', stderr: '', interrupted: false } };
  });
  await start($);
  await $.turn.start({ turnId: 'limited', text: 'work' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'limited', 'error');
  const pending = $.tool.call({ tool: 'Bash', command: 'foreground-work' });
  expect(await status($)).toMatchObject({
    ready: false, needs_continue: true, active_tools: 1, reason: 'tool call running',
  });
  releaseTool();
  await pending;
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, active_tools: 0 });
});

test('whitespace-only input does not block rate-limit continuation', async ($, on) => {
  const { control } = setup(on);
  await start($);
  await $.classic.StopFailure({ error: 'rate_limit' });
  control.draft = ' \t\n\u00a0';
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
});

// Three simulated minutes run about 240 heartbeats, which can take a while on
// a busy machine.
test('a switch waits for an idle session\'s background task at most three minutes', { timeoutMs: 30000 }, async ($, on) => {
  const { clock, control } = setup(on);
  on('tool.call', { tool: 'Bash' }, () => ({ result: {
    stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'bg1',
  } }));
  await start($);
  await $.tool.call({ tool: 'Bash', command: 'long-build', run_in_background: true });
  await $.classic.Stop({ stop_hook_active: false,
    background_tasks: [{ id: 'bg1', type: 'shell', status: 'running', description: 'wait for the build' }], session_crons: [] });
  control.action = 'prepare';
  control.generation = 'generation-1';
  await clock.advance(750);
  expect(await status($)).toMatchObject({ ready: false, reason: 'known background tasks remain', phase: 'prepare' });
  await clock.advance(2 * 60 * 1000);
  expect((await status($)).ready).toBe(false);
  await clock.advance(60 * 1000);
  expect(await status($)).toMatchObject({
    ready: true, needs_continue: false, stopped: ['background task bg1 (wait for the build)'],
  });
  // A released switch restarts the wait for the next one.
  control.action = 'run';
  control.generation = '';
  await clock.advance(750);
  expect(await status($)).toMatchObject({ ready: false, reason: 'known background tasks remain' });
});

test('a draft being edited still blocks rate-limit continuation', async ($, on) => {
  const { control } = setup(on);
  await start($);
  await $.classic.StopFailure({ error: 'rate_limit' });
  control.draft = 'unfinished';
  expect(await status($)).toMatchObject({ ready: false, needs_continue: true, reason: 'text draft is being edited' });
  control.draft = '';
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
});

// A switch used to wait for any draft until the old account recovered, even
// one left in a box nobody was looking at.
test('a draft holds a switch only while it is edited, then goes along with the exit', { timeoutMs: 30000 }, async ($, on) => {
  const { clock, control } = setup(on);
  let exits = 0;
  on('command.run', { command: 'exit' }, () => { exits += 1; return { text: '' }; });
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  control.draft = 'half written';
  await clock.advance(750);
  expect(await status($)).toMatchObject({ ready: false, reason: 'text draft is being edited' });
  control.draft = 'half written idea';
  await clock.advance(20000);
  expect((await status($)).ready).toBe(false);
  await clock.advance(11000);
  expect(await status($)).toMatchObject({ ready: true, draft_characters: 17 });
  control.action = 'exit';
  await clock.advance(750);
  expect(exits).toBe(1);
  const acks = control.requests.filter(request => request.path === '/exiting');
  expect(acks).toHaveLength(1);
  expect(acks[0]!.body).toMatchObject({ generation: 'generation-1', draft: 'half written idea' });
});

test('a draft too long to carry over keeps holding the switch', { timeoutMs: 30000 }, async ($, on) => {
  const { clock, control } = setup(on);
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  control.draft = 'x'.repeat(8001);
  await clock.advance(31000);
  expect(await status($)).toMatchObject({ ready: false, reason: 'text draft too long to keep across a restart' });
});

test('a draft the runner kept goes back into the input box once, as a quiet draft', async ($, on) => {
  const { clock, control } = setup(on);
  control.restore = 'half written idea';
  await start($);
  expect(control.draft).toBe('half written idea');
  // Delivered again until acknowledged, but put back only once.
  await clock.advance(750);
  expect(control.draft).toBe('half written idea');
  expect(control.requests.at(-1)!.body).toMatchObject({ restored: true });
  expect(await status($)).toMatchObject({ ready: true, draft_characters: 17 });
});

test('an interrupted session is not continued while its box has a draft', { timeoutMs: 30000 }, async ($, on) => {
  const { clock, control } = setup(on, 'continue');
  const submitted: string[] = [];
  on('prompt.submit', ($, e) => { submitted.push(e.text); return { text: e.text }; });
  await start($);
  control.draft = 'about to ask something';
  await clock.advance(31000);
  control.action = 'resume';
  await clock.advance(750);
  expect(submitted).toHaveLength(0);
  control.draft = '';
  await clock.advance(750);
  expect(submitted).toHaveLength(1);
});

// On 11-04 a message typed during a switch went back to the input box and
// blocked readiness until a release that only the finished switch could
// bring; the switch waited until the old account recovered.
test('a message typed during preparation is held without blocking the switch and sent on release', async ($, on) => {
  const { clock, control } = setup(on);
  const submitted: { text: string; origin: unknown }[] = [];
  on('prompt.submit', ($, e) => { submitted.push({ text: e.text, origin: e.origin }); return { text: e.text }; });
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  const result = await $.prompt.submit({ text: 'why is it stuck', wait: false, origin: { kind: 'composer' } });
  expect(result.drop).toContain('held');
  expect(control.draft).toBe('');
  expect(submitted).toHaveLength(0);
  expect(await status($)).toMatchObject({ ready: true, phase: 'prepare', held_messages: 1 });
  // Released without a restart: sent now, as the user's own message.
  control.action = 'run';
  control.generation = '';
  await clock.advance(750);
  expect(submitted).toEqual([{ text: 'why is it stuck', origin: { kind: 'plugin', name: 'qswitch-managed', asUser: true } }]);
  expect((await status($)).held_messages).toBe(0);
});

test('an exit hands held messages to the runner, and one typed after it updates the record', async ($, on) => {
  const { clock, control } = setup(on);
  let exits = 0;
  on('command.run', { command: 'exit' }, () => { exits += 1; return { text: '' }; });
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  await $.prompt.submit({ text: 'first', wait: false, origin: { kind: 'composer' } });
  control.action = 'exit';
  await clock.advance(750);
  expect(exits).toBe(1);
  const acks = control.requests.filter(request => request.path === '/exiting');
  expect(acks).toHaveLength(1);
  expect(acks[0]!.body).toMatchObject({ generation: 'generation-1', held: ['first'] });
  // Claude Code is shutting down: no heartbeat, only the record is updated.
  const before = control.requests.length;
  const result = await $.prompt.submit({ text: 'second', wait: false, origin: { kind: 'bridge' } });
  expect(result.drop).toContain('held');
  const later = control.requests.slice(before);
  expect(later.map(request => request.path)).toEqual(['/exiting']);
  expect(later[0]!.body).toMatchObject({ generation: 'generation-1', ready: true, held: ['first', 'second'] });
});

// The acknowledgement carried the draft; sending it after that empties the
// box, so the restarted session must not get the same text twice.
test('a draft sent after the exit acknowledgement is not put back as well', async ($, on) => {
  const { clock, control } = setup(on);
  on('command.run', { command: 'exit' }, () => ({ text: '' }));
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  control.draft = 'send me';
  await clock.advance(31000);
  control.action = 'exit';
  await clock.advance(750);
  const acks = () => control.requests.filter(request => request.path === '/exiting');
  expect(acks()[0]!.body).toMatchObject({ draft: 'send me' });
  control.draft = '';
  await $.prompt.submit({ text: 'send me', wait: false, origin: { kind: 'composer' } });
  expect(acks().at(-1)!.body).toMatchObject({ held: ['send me'], draft: '' });
});

test('a message too long to hold goes back to the input box', async ($, on) => {
  const { control } = setup(on);
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  const text = 'x'.repeat(6001);
  const result = await $.prompt.submit({ text, wait: false, origin: { kind: 'composer' } });
  expect(result.drop).toContain('back in the input box');
  expect(control.draft).toBe(text);
  expect(await status($)).toMatchObject({ ready: false, reason: 'text draft is being edited', held_messages: 0 });
});

test('accepted queued text still blocks rate-limit continuation until its turn finishes', async ($, on) => {
  setup(on);
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.turn.start({ turnId: 'limited', text: 'work' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'limited', 'error');
  await $.prompt.submit({ text: 'queued work', wait: true, origin: { kind: 'composer' } });
  expect(await status($)).toMatchObject({ ready: false, needs_continue: true, queued_submissions: 1 });
  await $.turn.start({ turnId: 'queued-turn', text: 'queued work' });
  expect(await status($)).toMatchObject({ ready: false, needs_continue: true, queued_submissions: 0 });
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
  // Any heartbeat would revoke the runner's exit acknowledgement. Past the
  // grace the session stays quiet: the runner ends an overdue exit itself.
  await clock.advance(10000);
  expect(control.posted).toBe(afterExit);
  await clock.advance(80000);
  expect(control.posted).toBe(afterExit);
  expect(await status($)).toMatchObject({ ready: false, reason: 'waiting for Claude Code to exit', exit_overdue: true });
});

test('an acknowledged exit Claude Code runs late still proceeds', async ($, on) => {
  const { clock, control } = setup(on);
  let exits = 0;
  on('command.run', { command: 'exit' }, () => { exits += 1; return { text: '' }; });
  await start($);
  control.action = 'exit';
  control.generation = 'generation-1';
  await clock.advance(750);
  expect(exits).toBe(1);
  // Claude Code held the first /exit in its queue and runs it 10 s later.
  await clock.advance(10000);
  const late = await $.command.run({
    command: 'exit', args: '', origin: { kind: 'composer' },
    presentation: { isFullscreen: false, columns: 80 },
  });
  expect(late.text ?? '').not.toContain('cancelled');
  expect(exits).toBe(2);
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

test('an automatic delivery during preparation is admitted and only delays readiness', async ($, on) => {
  const { control } = setup(on, 'continue');
  on('prompt.submit', ($, e) => ({ text: e.text }));
  control.action = 'prepare';
  control.generation = 'generation-1';
  await start($);
  const text = '<task-notification>\n<task-id>bg9</task-id>\n<status>completed</status>\n</task-notification>';
  const result = await $.prompt.submit({ text, wait: true, origin: { kind: 'task-notification', producer: 'session-task' } });
  expect(result.drop).toBeUndefined();
  expect((await status($)).ready).toBe(false);
  await $.turn.start({ turnId: 'notified', text });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'notified', 'error');
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true, phase: 'prepare' });
});

test('text queued during a turn and absorbed into it does not block readiness', async ($, on) => {
  const { clock } = setup(on);
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.turn.start({ turnId: 'working', text: 'work' });
  // Typed while the turn runs; Claude Code feeds it into the running turn.
  await $.prompt.submit({ text: 'also check this', wait: true, origin: { kind: 'composer' } });
  await complete($, 'working', 'answer');
  expect((await status($)).ready).toBe(false);
  await clock.advance(5000);
  expect(await status($)).toMatchObject({ ready: true, queued_submissions: 0 });
});

test('a queued prompt that starts its own turn keeps blocking until it finishes', async ($, on) => {
  const { clock } = setup(on);
  on('prompt.submit', ($, e) => ({ text: e.text }));
  await start($);
  await $.turn.start({ turnId: 'first', text: 'work' });
  await $.prompt.submit({ text: 'next task', wait: true, origin: { kind: 'composer' } });
  await complete($, 'first', 'answer');
  await $.turn.start({ turnId: 'second', text: 'next task' });
  await clock.advance(5000);
  expect((await status($)).ready).toBe(false);
  await complete($, 'second', 'answer');
  expect((await status($)).ready).toBe(true);
});

test('a session restarted while interrupted continues once when the account recovers', async ($, on) => {
  const { clock, control } = setup(on, 'continue');
  const submitted: string[] = [];
  on('prompt.submit', ($, e) => { submitted.push(e.text); return { text: e.text }; });
  await start($);
  control.action = 'resume';
  await clock.advance(750);
  control.action = 'run';
  expect(submitted).toHaveLength(1);
  expect(submitted[0]).toContain('Continue the work interrupted by the account usage limit');
  await $.turn.start({ turnId: 'resumed', text: submitted[0]! });
  await complete($, 'resumed', 'answer');
  control.action = 'resume';
  await clock.advance(750);
  expect(submitted).toHaveLength(1);
});

test('a session restarted with the continuation on its command line is not continued again', async ($, on) => {
  const { clock, control } = setup(on, 'prompted');
  const submitted: string[] = [];
  on('prompt.submit', ($, e) => { submitted.push(e.text); return { text: e.text }; });
  await start($);
  expect(await status($)).toMatchObject({ ready: true, needs_continue: true });
  control.action = 'resume';
  await clock.advance(750);
  control.action = 'run';
  expect(submitted).toHaveLength(0);
});

test('a rate limit hit in the running process keeps Claude Code\'s own automatic continue', async ($, on) => {
  const { clock, control } = setup(on);
  const submitted: string[] = [];
  on('prompt.submit', ($, e) => { submitted.push(e.text); return { text: e.text }; });
  await start($);
  await $.turn.start({ turnId: 'limited', text: 'work' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'limited', 'error');
  control.action = 'resume';
  await clock.advance(750);
  expect(submitted).toHaveLength(0);
});

test('every main-turn rate limit is counted, even when already interrupted', async ($, on) => {
  setup(on, 'continue');
  await start($);
  expect((await status($)).limit_hits).toBe(0);
  await $.turn.start({ turnId: 'again', text: 'continue' });
  await $.classic.StopFailure({ error: 'rate_limit' });
  await complete($, 'again', 'error');
  await $.classic.StopFailure({ error: 'rate_limit', agent_id: 'child' });
  expect(await status($)).toMatchObject({ limit_hits: 1, needs_continue: true });
});
