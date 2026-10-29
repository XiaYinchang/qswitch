// Claude Code 2.1.312 function hooks. Only submitted transcript state is
// preserved: prompt.read() does not expose attachments or every temporary UI.
  let url = '';
  let token = '';
  let phase = 'run';
  let generation = '';
  let initialized = false;
  let needsContinue = false;
  let unknown = '';
  let blockedSubmission = false;
  let revision = 0;
  let polling = false;
  let requestPending = false;
  let exiting = false;
  let mainFailure;
  let timer;
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
      if (!['run', 'prepare', 'exit'].includes(value.action)) throw new Error('invalid coordinator action');
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
    let reason = '';
    if (!initialized) reason = 'initializing';
    else if (unknown) reason = unknown;
    else if (blockedSubmission) reason = 'submission attempted during account switch; submit again after release';
    else if (turns.size) reason = 'model turn running';
    else if (tools.size) reason = 'tool call running';
    else if (queued.length) reason = 'submitted prompt has not started a known turn';
    else if (uncertainTasks.size) reason = 'background task lifecycle not confirmed by Stop';
    else if (background.length) reason = 'known background tasks remain';
    else if (crons.length) reason = 'scheduled session tasks remain';
    else if (agents.some(agent => !terminal(agent.status))) reason = 'known agent is not finished';
    else if (draft.text.length) reason = 'text draft is present';
    else if (before !== revision) reason = 'session changed while checking readiness';
    else if (!sessionId || !cwd) reason = 'session identity unavailable';
    return {
      session_id: sessionId,
      cwd,
      ready: !reason,
      needs_continue: needsContinue,
      reason,
    };
  }

  async function poll($) {
    if (polling || exiting || !url || !token) return;
    polling = true;
    try {
      lastState = await snapshot($);
      const before = revision;
      const action = await request($, '/state', lastState);
      if (action.action === 'run') {
        if (phase !== 'run') {
          phase = 'run';
          generation = '';
          blockedSubmission = false;
          changed();
        }
        return;
      }
      if (generation && generation !== action.generation) {
        unknown = 'coordinator changed generation without releasing preparation';
        changed();
        return;
      }
      phase = 'prepare';
      generation = action.generation;
      if (action.action !== 'exit' || before !== revision) return;
      const preparedGeneration = generation;
      const candidate = await snapshot($);
      if (!candidate.ready) { lastState = candidate; return; }
      const candidateRevision = revision;
      const acknowledged = await request($, '/exiting', { ...candidate, generation: preparedGeneration });
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
      // A plugin that intercepts /exit must not leave this process eligible.
      unknown = 'exit command returned without terminating the session';
      exiting = false;
      changed();
      await request($, '/state', { ...finalState, ready: false, reason: unknown });
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
    if (value.isError) return;
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
    if (phase !== 'run') {
      blockedSubmission = true;
      const message = 'qswitch is preparing an account switch. This submission was not sent. Submit it again after preparation is released.';
      if (e.origin.kind === 'composer' || e.origin.kind === 'bridge') {
        try {
          const filled = await $.prompt.fill({ text: e.text, mode: 'append' });
          if (!filled.isFilled) unknown = 'blocked submission could not be restored to the text input';
        } catch {
          unknown = 'blocked submission could not be restored to the text input';
        }
      } else {
        // Automatic deliveries cannot be silently dropped then forgotten.
        unknown = 'automatic delivery blocked during preparation; manual attention required';
      }
      return { drop: message };
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
    blockedSubmission = true;
    changed();
    return next.called ? next(e) : { drop: 'qswitch could not safely admit this prompt; submit again after checking /qswitch-status.' };
  });

  on('turn.start', ($, e, next) => {
    turns.add(e.turnId);
    if (e.agentId === undefined) {
      queued.shift();
      mainFailure = undefined;
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
    changed();
    return result;
  });

  on('classic.StopFailure', ($, e, next) => {
    if (e.agent_id === undefined) {
      mainFailure = e.error;
      needsContinue = e.error === 'rate_limit';
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
    if (initial === 'continue') needsContinue = true;
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
      ...lastState, phase, generation, active_turns: turns.size, active_tools: tools.size,
      queued_submissions: queued.length, uncertain_tasks: [...uncertainTasks],
      background_tasks: background, session_crons: crons,
      ui_contract: 'submitted transcript preserved; unsubmitted attachments and temporary UI are not guaranteed',
    }, null, 2) };
  });
}
