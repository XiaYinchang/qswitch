// Claude Code 2.1.312 function hooks. Only submitted transcript state is
// preserved: prompt.read() does not expose attachments or every temporary UI.
const EXIT_GRACE_MS = 30000;
const QUEUE_SETTLE_MS = 5000;
// A switch waits this long for an idle session's background tasks and agents
// to finish; then the session exits with them unfinished and its continuation
// lists them. The account is used up, so every other session waits as well. On
// 11-04 two waits ran out a ten-minute limit for work that could not finish in
// it: a hung ssh, and a command sleeping until a fixed time.
const ASYNC_WAIT_MS = 3 * 60 * 1000;
// Same text the runner passes on restart after an account switch.
const CONTINUATION = 'Continue the work interrupted by the account usage limit from the last unfinished step. Do not repeat work already completed.';
// Input typed while a switch drains the sessions is held up to this many
// characters, which keeps the exit acknowledgement within the runner's limit.
const HELD_LIMIT = 6000;
const HELD_NOTICE = 'qswitch is switching accounts. This message is held and will be sent as soon as the switch finishes.';
// A draft left unchanged this long no longer holds a switch: the exit hands it
// to the runner, which puts it back in the input box after the restart. A
// draft longer than DRAFT_LIMIT characters is not carried and keeps holding.
const DRAFT_QUIET_MS = 30 * 1000;
const DRAFT_LIMIT = 8000;
  let url = '';
  let token = '';
  let phase = 'run';
  let generation = '';
  let initialized = false;
  let needsContinue = false;
  // True only when this process started already interrupted. Claude Code's
  // own automatic continue was cancelled by that restart; a rate limit hit in
  // this process keeps the official automatic continue instead.
  let restartedInterrupted = false;
  // Counts main-turn rate-limit failures so the runner can signal each one,
  // even when needs_continue was already true from an earlier interruption.
  let limitHits = 0;
  let unknown = '';
  // Messages typed during a switch, in order, not yet sent.
  let held = [];
  // The input box as last read, and when its text last changed.
  let draftText = '';
  let draftChangedAt = -Infinity;
  // The draft the previous process left was put back in this one's box.
  let restored = false;
  let revision = 0;
  let polling = false;
  let requestPending = false;
  let exiting = false;
  let awaitingExit = false;
  let exitOverdue = false;
  let asyncWaitOver = false;
  let asyncTimer;
  let exitTimer;
  let mainFailure;
  let timer;
  let queueSettle;
  let lastState = { ready: false, reason: 'initializing' };
  const turns = new Set();
  const tools = new Set();
  const queued = [];
  const uncertainTasks = new Set();
  let background = [];
  let crons = [];

  const changed = () => { revision += 1; };
  const terminal = status => ['completed', 'failed', 'killed'].includes(status);

  async function request($, path, state) {
    if (requestPending) throw new Error('previous coordinator request still pending');
    requestPending = true;
    let deadline;
    const pending = $.http.fetch(url + path, {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(state),
    }).finally(() => { requestPending = false; });
    try {
      const response = await Promise.race([
        pending,
        new Promise((_, reject) => {
          deadline = $.clock.after(2000, () => reject(new Error('coordinator request timed out')));
        }),
      ]);
      if (!response.ok) throw new Error(`coordinator returned HTTP ${response.status}`);
      const value = JSON.parse(response.text);
      if (!['run', 'prepare', 'exit', 'resume'].includes(value.action)) throw new Error('invalid coordinator action');
      if (value.action === 'resume') return value;
      if (value.action !== 'run' && (typeof value.generation !== 'string' || !value.generation)) {
        throw new Error('coordinator omitted generation');
      }
      return value;
    } finally {
      deadline?.cancel();
    }
  }

  async function snapshot($) {
    const before = revision;
    const sessionId = await $.session.id();
    const cwd = await $.session.cwd();
    const agents = await $.agent.list();
    const draft = await $.prompt.read();
    const now = await $.clock.now();
    const text = String(draft?.text ?? '');
    if (text !== draftText) {
      draftText = text;
      draftChangedAt = now;
    }
    const hasDraft = text.trim().length > 0;
    let reason = '';
    // Asynchronous work blocks an ordinary session. A pending continuation may
    // need to run to retire that work, and a switch waits for it only so long;
    // keep reporting it without blocking in either case.
    const asyncWaived = needsContinue || asyncWaitOver;
    if (!initialized) reason = 'initializing';
    else if (awaitingExit || exitOverdue) reason = 'waiting for Claude Code to exit';
    else if (unknown) reason = unknown;
    else if (turns.size) reason = 'model turn running';
    else if (tools.size) reason = 'tool call running';
    else if (queued.length) reason = 'submitted prompt has not started a known turn';
    else if (!asyncWaived && uncertainTasks.size) reason = 'background task lifecycle not confirmed by Stop';
    else if (!asyncWaived && background.length) reason = 'known background tasks remain';
    // A scheduled task never finishes, so a switch does not wait for one: the
    // exit stops it and the continuation lists it. On 11-04 a switch waited
    // ten minutes for a task due the next day. An upgrade restart tells the
    // session nothing, so it still waits.
    else if (!asyncWaived && crons.length && phase !== 'prepare') reason = 'scheduled session tasks remain';
    else if (!asyncWaived && agents.some(agent => !terminal(agent.status))) reason = 'known agent is not finished';
    // A draft holds the switch only while someone is typing it; a quiet one
    // goes along with the exit. A switch used to wait for any draft until the
    // old account recovered, even one left in a box nobody was looking at.
    else if (hasDraft && text.length > DRAFT_LIMIT) reason = 'text draft too long to keep across a restart';
    else if (hasDraft && now - draftChangedAt < DRAFT_QUIET_MS) reason = 'text draft is being edited';
    else if (before !== revision) reason = 'session changed while checking readiness';
    else if (!sessionId || !cwd) reason = 'session identity unavailable';
    return {
      session_id: sessionId,
      cwd,
      ready: !reason,
      needs_continue: needsContinue,
      limit_hits: limitHits,
      reason,
      stopped: unfinishedWork(agents),
      restored,
    };
  }

  // The draft an exit hands to the runner.
  const carriedDraft = () => (draftText.trim() ? draftText.replace(/\u0000/g, '') : '');

  // Puts back the draft the previous process had when the switch restarted
  // it. The text arrives from the runner, so it is not a new edit.
  async function restoreDraft($, text) {
    try {
      const filled = await $.prompt.fill({ text, mode: 'append' });
      if (!filled.isFilled) return;
      draftText = String(filled.text ?? text);
      restored = true;
      changed();
    } catch {}
  }

  // What an exit now would stop. The continuation after the restart lists it,
  // since none of it reports back to the new process.
  function unfinishedWork(agents) {
    const items = [];
    for (const task of background) {
      items.push(task.description ? `background task ${task.id} (${task.description})` : `background task ${task.id}`);
    }
    for (const agent of agents) {
      if (terminal(agent.status)) continue;
      items.push(agent.description ? `agent ${agent.id} (${agent.description})` : `agent ${agent.id}`);
    }
    for (const cron of crons) items.push(`scheduled task ${cron.id}`);
    if (uncertainTasks.size) items.push(`${uncertainTasks.size} tool call(s) whose background work was not confirmed`);
    return items.slice(0, 20).map(item => String(item).slice(0, 200));
  }

  async function restoreToBox($, text) {
    try {
      const filled = await $.prompt.fill({ text, mode: 'append' });
      if (!filled.isFilled) unknown = 'held submission could not be restored to the text input';
    } catch {
      unknown = 'held submission could not be restored to the text input';
    }
    changed();
  }

  // Holds input typed while a switch drains the sessions, so the switch never
  // waits for the user: the runner sends it as the restarted session's prompt,
  // and a preparation released without a restart sends it here. Returning it
  // to the text box and blocking readiness until the release waited for a
  // release that only the finished switch could bring: on 11-04 a message
  // typed at 05:27 kept the switch queued at 05:17 waiting until the old
  // account recovered at 05:50.
  async function hold($, input, origin) {
    const text = String(input ?? '').replace(/\u0000/g, '');
    if (!text.trim()) return { drop: HELD_NOTICE };
    if (held.reduce((n, item) => n + item.length, 0) + text.length > HELD_LIMIT) {
      // Too long to carry over. In the text box it holds the switch until it
      // is sent or cleared, as any draft does.
      await restoreToBox($, text);
      return { drop: 'qswitch is switching accounts and cannot hold a message this long. It is back in the input box: send it after the switch, or clear it to let the switch go ahead.' };
    }
    held.push(text);
    changed();
    // After the exit acknowledgement only the runner's record of it may be
    // updated: a heartbeat would revoke the acknowledgement. A message typed
    // here emptied the input box, so the draft recorded with the
    // acknowledgement is gone; one from the remote bridge left the box alone.
    if (exiting && !requestPending) {
      const draft = origin === 'composer' ? '' : carriedDraft();
      try { await request($, '/exiting', { ...lastState, generation, held, draft }); } catch {}
    }
    return { drop: HELD_NOTICE };
  }

  // Sends what was held during a preparation that ended without a restart, as
  // the user's own message. Text that cannot be sent goes back to the box.
  async function sendHeld($) {
    const text = held.join('\n\n');
    held = [];
    if (!text.trim()) return;
    changed();
    try {
      await $.prompt.submit({ text, asUser: true, wait: false });
    } catch {
      await restoreToBox($, text);
    }
  }

  async function poll($) {
    if (polling || exiting || !url || !token) return;
    polling = true;
    try {
      lastState = await snapshot($);
      const before = revision;
      const action = await request($, '/state', lastState);
      if (typeof action.restore === 'string' && action.restore && !restored) await restoreDraft($, action.restore);
      if (action.action === 'resume') {
        // The account recovered without a switch. Continue only a session that
        // lost Claude Code's own automatic continue, and only if nothing
        // changed since this heartbeat and nobody has a draft in its box.
        if (phase !== 'run' || before !== revision || !lastState.ready || !needsContinue || !restartedInterrupted || draftText.trim()) return;
        restartedInterrupted = false;
        changed();
        await $.prompt.submit({ text: CONTINUATION, wait: false });
        return;
      }
      if (action.action === 'run') {
        if (phase !== 'run') {
          phase = 'run';
          generation = '';
          asyncTimer?.cancel();
          asyncTimer = undefined;
          asyncWaitOver = false;
          changed();
          await sendHeld($);
        }
        return;
      }
      if (generation && generation !== action.generation) {
        unknown = 'coordinator changed generation without releasing preparation';
        changed();
        return;
      }
      if (phase !== 'prepare') {
        asyncTimer?.cancel();
        asyncWaitOver = false;
        asyncTimer = $.clock.after(ASYNC_WAIT_MS, () => { asyncWaitOver = true; changed(); });
      }
      phase = 'prepare';
      generation = action.generation;
      if (action.action !== 'exit' || before !== revision) return;
      const preparedGeneration = generation;
      const candidate = await snapshot($);
      if (!candidate.ready) { lastState = candidate; return; }
      const candidateRevision = revision;
      const acknowledged = await request($, '/exiting', { ...candidate, generation: preparedGeneration, held, draft: carriedDraft() });
      if (acknowledged.action !== 'exit' || acknowledged.generation !== preparedGeneration) return;
      const finalState = await snapshot($);
      lastState = finalState;
      if (!finalState.ready || candidateRevision !== revision || phase !== 'prepare' ||
          generation !== preparedGeneration || finalState.session_id !== candidate.session_id ||
          finalState.cwd !== candidate.cwd) {
        await request($, '/state', { ...finalState, ready: false, reason: 'exit acknowledgement cancelled: session changed' });
        return;
      }
      exiting = true;
      await $.command.run({ command: 'exit' });
      // Claude Code 2.1.312 returns from /exit while shutdown is still running,
      // and can also hold /exit in its input queue. Stay silent either way: a
      // heartbeat would revoke the runner's acknowledgement. The runner ends a
      // session whose acknowledged exit is overdue.
      awaitingExit = true;
      changed();
      exitTimer = $.clock.after(EXIT_GRACE_MS, () => {
        awaitingExit = false;
        exitOverdue = true;
        changed();
      });
    } catch (error) {
      lastState = { ...lastState, ready: false, reason: String(error) };
      exiting = false;
      // A timed-out /exiting may still reach the runner. The next heartbeat
      // revokes that acknowledgement before any new exit can be attempted.
    } finally {
      polling = false;
    }
  }

  // Task-producing tools must have an understood outcome or a later Stop
  // snapshot. StopFailure deliberately cannot clear these: it has no snapshot.
  function recordTool(e, value) {
    const key = e.tool_use_id;
    if (value.deny) { uncertainTasks.delete(key); return; }
    // A failed Bash call (non-zero exit, rejected command) ran in the
    // foreground; backgrounding is reported as success with a task id.
    if (value.isError) { if (e.tool === 'Bash') uncertainTasks.delete(key); return; }
    const result = value.result;
    if (!result || typeof result !== 'object') return;
    if (e.tool === 'Bash' && typeof result.stdout === 'string' && typeof result.stderr === 'string') {
      uncertainTasks.delete(key);
      if (result.backgroundTaskId) background.push({ id: result.backgroundTaskId, type: 'shell' });
      else if (result.backgroundedByUser || result.backgroundedByTurnAbort ||
               result.backgroundedToDeliverMessage || result.timedOutAfterMs) uncertainTasks.add(key);
    } else if (e.tool === 'Agent' || e.tool === 'Skill') {
      if (result.status === 'completed' || (result.status === 'forked' && !result.background)) {
        uncertainTasks.delete(key);
      }
      // A launched remote agent has no local lifecycle. Even a disappeared
      // local agent needs a Stop snapshot before its launch can be forgotten.
    } else if (e.tool === 'TaskStop' && typeof result.task_id === 'string') {
      background = background.filter(task => task.id !== result.task_id);
      uncertainTasks.delete(key);
    } else if (e.tool === 'CronCreate' && typeof result.id === 'string') {
      crons.push({ id: result.id });
      uncertainTasks.delete(key);
    } else if (e.tool === 'CronDelete' && typeof result.id === 'string') {
      crons = crons.filter(cron => cron.id !== result.id);
      uncertainTasks.delete(key);
    }
  }

  const synchronousTools = new Set([
    'Read', 'Write', 'Edit', 'MultiEdit', 'Glob', 'Grep', 'NotebookEdit',
    'WebFetch', 'WebSearch', 'ToolSearch', 'AskUserQuestion', 'TodoWrite',
    'TaskCreate', 'TaskGet', 'TaskList', 'TaskUpdate', 'CronList',
    'EnterPlanMode', 'ExitPlanMode', 'ListAgents', 'GetTask',
  ]);

// Claude Code delivers a finished background task as a task-notification
// prompt. When that turn ends in a rate limit there is no Stop snapshot, so
// the notification is the only completion evidence.
function finishedTaskID(e) {
  if (e.origin?.kind !== 'task-notification' || typeof e.text !== 'string') return '';
  const id = /<task-id>([^<]+)<\/task-id>/.exec(e.text)?.[1];
  const status = /<status>([^<]+)<\/status>/.exec(e.text)?.[1];
  return id && ['completed', 'failed', 'killed'].includes(status) ? id : '';
}

export function register(on) {
  on('tool.call', async ($, e, next) => {
    tools.add(e.tool_use_id);
    if (!synchronousTools.has(e.tool)) uncertainTasks.add(e.tool_use_id);
    changed();
    try {
      const result = await next(e);
      recordTool(e, result);
      return result;
    } catch (error) {
      uncertainTasks.add(e.tool_use_id);
      throw error;
    } finally {
      tools.delete(e.tool_use_id);
      changed();
    }
  });

  on('agent.spawn', async ($, e, next) => {
    const key = {};
    uncertainTasks.add(key);
    changed();
    const result = await next(e);
    if (result.deny) uncertainTasks.delete(key);
    else uncertainTasks.add(key);
    // A Stop observed while this spawn was pending cannot confirm its outcome.
    changed();
    return result;
  });

  on('prompt.submit', async ($, e, next) => {
    changed();
    // Typed input is held for after the switch. Automatic deliveries (task
    // notifications, scheduled prompts) cannot be held, so they are admitted:
    // the queued prompt and its turn block readiness until they finish, and
    // the drain resumes afterwards.
    if (phase !== 'run' && (e.origin.kind === 'composer' || e.origin.kind === 'bridge')) {
      return hold($, e.text, e.origin.kind);
    }
    const submission = {};
    // Typed prompts queue even with turnId; machine notifications may instead
    // enter the currently running turn. Count every other admission first.
    const deliveredInTurn = e.turnId && e.origin.kind !== 'composer' && e.origin.kind !== 'bridge' && !e.wait;
    if (!deliveredInTurn) queued.push(submission);
    try {
      const result = await next(e);
      if (result.drop) {
        const index = queued.indexOf(submission);
        if (index !== -1) queued.splice(index, 1);
      } else {
        const finished = finishedTaskID(e);
        if (finished) background = background.filter(task => task.id !== finished);
      }
      return result;
    } catch (error) {
      unknown = 'submission lifecycle did not settle';
      throw error;
    } finally {
      changed();
    }
  }).catch(($, e, next) => {
    unknown = 'prompt admission hook failed';
    changed();
    return next.called ? next(e) : { drop: 'qswitch could not safely admit this prompt; submit again after checking /qswitch-status.' };
  });

  on('turn.start', ($, e, next) => {
    turns.add(e.turnId);
    if (e.agentId === undefined) {
      queued.shift();
      mainFailure = undefined;
      queueSettle?.cancel();
      queueSettle = undefined;
    }
    changed();
    return next(e);
  });

  on('turn.complete', async ($, e, next) => {
    const result = await next(e);
    turns.delete(e.turnId);
    if (e.agentId === undefined && (e.reason === 'answer' || e.reason === 'aborted')) needsContinue = false;
    if (e.agentId === undefined && e.reason === 'error' && mainFailure === undefined) {
      unknown = 'model error ended without a known StopFailure outcome';
    }
    // Text queued while a turn runs is often absorbed into that turn instead
    // of starting its own. A queued prompt that still has to run starts its
    // turn right after this one ends; if none starts within the grace period,
    // every remaining submission was consumed.
    if (e.agentId === undefined && queued.length) {
      queueSettle?.cancel();
      queueSettle = $.clock.after(QUEUE_SETTLE_MS, () => {
        queueSettle = undefined;
        if (turns.size === 0) {
          queued.length = 0;
          changed();
        }
      });
    }
    changed();
    return result;
  });

  on('classic.StopFailure', ($, e, next) => {
    if (e.agent_id === undefined) {
      mainFailure = e.error;
      needsContinue = e.error === 'rate_limit';
      if (e.error === 'rate_limit') limitHits += 1;
      restartedInterrupted = false;
      if (unknown === 'model error ended without a known StopFailure outcome') unknown = '';
    }
    changed();
    return next(e);
  });

  on('classic.Stop', async ($, e, next) => {
    if (e.agent_id !== undefined) return next(e);
    const before = revision;
    const result = await next(e);
    // The snapshot predates hooks below us. A new tool or delivery while they
    // run invalidates it; wait for a later Stop instead of clearing that work.
    if (before === revision) {
      if (Array.isArray(e.background_tasks)) {
        background = [...e.background_tasks];
        if (Array.isArray(e.session_crons) && tools.size === 0) uncertainTasks.clear();
      }
      if (Array.isArray(e.session_crons)) crons = [...e.session_crons];
    }
    changed();
    return result;
  });

  on('prompt.edit', ($, e, next) => { changed(); return next(e); });
  on('prompt.fill', ($, e, next) => { changed(); return next(e); });
  on('prompt.change', ($, e, next) => { changed(); return next(e); });
  on('session.receive', ($, e, next) => {
    uncertainTasks.add('incoming session delivery');
    changed();
    return next(e);
  });

  on('command.run', { command: 'exit' }, async ($, e, next) => {
    if (!exiting) return next(e);
    // The acknowledged exit runs now, possibly after Claude Code queued it;
    // waiting for it is no reason to cancel it.
    awaitingExit = false;
    exitOverdue = false;
    exitTimer?.cancel();
    const before = revision;
    const state = await snapshot($);
    if (!state.ready || before !== revision || phase !== 'prepare' || !generation) {
      exiting = false;
      await request($, '/state', { ...state, ready: false, reason: 'exit acknowledgement cancelled: session changed' });
      return { text: 'qswitch cancelled automatic exit because session state changed.' };
    }
    return next(e);
  }).catch(($, e, next) => {
    if (next.called) return next(e);
    unknown = 'automatic exit readiness check failed';
    exiting = false;
    changed();
    return { text: 'qswitch cancelled automatic exit: readiness could not be confirmed.' };
  });

  on('session.start', async ($, e, next) => {
    timer?.cancel();
    await $.command.register({
      name: 'qswitch-status',
      description: 'Show managed session readiness and account-switch blockers',
    });
    url = await $.env.get('QSWITCH_CONTROL_URL') ?? '';
    token = await $.env.get('QSWITCH_CONTROL_TOKEN') ?? '';
    const initial = await $.env.get('QSWITCH_INITIAL_CONTINUATION');
    // SessionMessage and ApiMessage expose no SDK assistant error in 2.1.312.
    // Adoption must explicitly identify idle versus rate-limit interruption.
    // "prompted": the runner put the continuation on the command line, so a
    // later resume must not send a second one.
    if (initial === 'continue') { needsContinue = true; restartedInterrupted = true; }
    else if (initial === 'prompted') needsContinue = true;
    else if (initial !== 'idle') unknown = 'initial continuation state was not explicitly supplied';
    if (!/^http:\/\/127\.0\.0\.1:[0-9]+$/.test(url) || !token) {
      unknown = 'private loopback coordinator credentials unavailable';
      url = '';
    }
    if (!e.isInteractive || e.surface !== 'terminal') unknown = 'managed rotation requires an interactive terminal';
    const result = await next(e);
    initialized = true;
    changed();
    await poll($);
    timer = $.clock.every(750, () => { void poll($); });
    return result;
  });

  on('command.run', { command: 'qswitch-status' }, async ($) => {
    try { lastState = await snapshot($); }
    catch (error) { lastState = { ...lastState, ready: false, reason: String(error) }; }
    return { text: JSON.stringify({
      ...lastState, phase, generation, exit_overdue: exitOverdue, active_turns: turns.size, active_tools: tools.size,
      queued_submissions: queued.length, held_messages: held.length, draft_characters: draftText.length,
      uncertain_tasks: [...uncertainTasks],
      background_tasks: background, session_crons: crons,
      ui_contract: 'submitted transcript preserved; unsubmitted attachments and temporary UI are not guaranteed',
    }, null, 2) };
  });
}
