import test from 'node:test';
import assert from 'node:assert/strict';
import { LiveConnection } from '../assets/live.js';
import { resolveRoute, roomPath, editorPath, gamePath } from '../assets/routes.js';
import { validQuiz, quizApp, quizPrompt } from '../assets/app.js';
import { messages } from '../assets/i18n.js';

function harness() {
  let now = 10000;
  let nextID = 0;
  const timers = new Map();
  const sockets = [];
  const states = [];
  const statuses = [];
  let expirations = 0;
  class Socket {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      sockets.push(this);
    }
    close() {
      this.readyState = 3;
      this.onclose?.();
    }
    state(sequence = 1) {
      this.readyState = 1;
      this.onmessage?.({
        data: JSON.stringify({
          kind: 'state',
          snapshot: {
            code: 'ABCDEFGH',
            sequence,
            phase: 'lobby',
            revision: 1,
            players: [],
          },
        }),
      });
    }
    heartbeat() {
      this.onmessage?.({ data: '{"kind":"heartbeat"}' });
    }
  }
  const env = {
    Date: { now: () => now },
    Math: { random: () => 0 },
    document: { hidden: false },
    navigator: { onLine: true },
    location: { href: 'https://kahut.example/' },
    WebSocket: Socket,
    AbortController,
    fetch: async () => ({ status: 200 }),
    setTimeout(fn, delay) {
      const id = ++nextID;
      timers.set(id, { fn, at: now + delay });
      return id;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    setInterval(fn, delay) {
      const id = ++nextID;
      timers.set(id, { fn, at: now + delay, interval: delay });
      return id;
    },
    clearInterval(id) {
      timers.delete(id);
    },
  };
  const live = new LiveConnection({
    env,
    onState: (state) => states.push(state),
    onStatus: (status) => {
      if (status) assert.ok(states.length);
      statuses.push(status);
    },
    onExpired: () => expirations++,
  });
  return {
    env,
    live,
    sockets,
    states,
    statuses,
    timers,
    expired: () => expirations,
    advance(ms) {
      const target = now + ms;
      for (;;) {
        const entry = [...timers.entries()]
          .filter(([, timer]) => timer.at <= target)
          .sort((a, b) => a[1].at - b[1].at)[0];
        if (!entry) break;
        const [id, timer] = entry;
        now = timer.at;
        if (timer.interval) timer.at += timer.interval;
        else timers.delete(id);
        timer.fn();
      }
      now = target;
    },
    suspend(ms) {
      now += ms;
    },
  };
}

test('page paths resolve to distinct views and legacy invitations become path links', () => {
  for (const [path, search, want] of [
    ['/', '', { kind: 'home', path: '/' }],
    ['/host', '', { kind: 'host', path: '/host' }],
    ['/host/quizzes/new', '', { kind: 'newQuiz', path: '/host/quizzes/new' }],
    [
      '/host/quizzes/quiz-1/edit',
      '',
      { kind: 'editQuiz', quizId: 'quiz-1', path: '/host/quizzes/quiz-1/edit' },
    ],
    [
      '/host/games/abcdefgh',
      '',
      { kind: 'history', code: 'ABCDEFGH', path: '/host/games/ABCDEFGH' },
    ],
    [
      '/rooms/abcdefgh',
      '?resume=1',
      { kind: 'room', code: 'ABCDEFGH', path: '/rooms/ABCDEFGH' },
    ],
    ['/resume', '', { kind: 'resume', path: '/resume' }],
    [
      '/',
      '?room=abcdefgh&resume=1',
      { kind: 'room', code: 'ABCDEFGH', path: '/rooms/ABCDEFGH' },
    ],
    ['/', '?resume=1', { kind: 'resume', path: '/resume' }],
    [
      '/host/quizzes/quiz-1/edit/',
      '',
      { kind: 'editQuiz', quizId: 'quiz-1', path: '/host/quizzes/quiz-1/edit' },
    ],
    ['/unknown', '', { kind: 'notFound', path: '/unknown' }],
  ])
    assert.deepEqual(resolveRoute(path, search), want);
  assert.equal(roomPath('abcdefgh'), '/rooms/ABCDEFGH');
  assert.equal(editorPath('quiz-1'), '/host/quizzes/quiz-1/edit');
  assert.equal(editorPath(), '/host/quizzes/new');
  assert.equal(gamePath('abcdefgh'), '/host/games/ABCDEFGH');
});

test('first state enables readiness; socket open and heartbeat do not', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  assert.match(h.sockets[0].url, /^wss:/);
  h.sockets[0].readyState = 1;
  h.sockets[0].heartbeat();
  assert.equal(h.live.ready, false);
  h.sockets[0].state();
  assert.equal(h.live.ready, true);
  h.advance(2200);
  assert.equal(h.sockets.length, 1);
  h.live.stop();
  assert.equal(h.timers.size, 0);
});

test('overlapping recovery events preserve pending attempts for two seconds', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  h.advance(1200);
  h.live.recover();
  h.live.recover();
  h.live.recover();
  assert.equal(h.sockets.length, 1);
  h.advance(800);
  assert.equal(h.live.ready, false);
  h.advance(250);
  assert.equal(h.sockets.length, 2);
  h.sockets[1].state();
  assert.equal(h.live.ready, true);
  h.live.stop();
});

test('one failure produces one retry and stale callbacks cannot replace state', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  const old = h.sockets[0];
  old.onerror();
  old.onclose();
  h.advance(250);
  assert.equal(h.sockets.length, 2);
  old.state(99);
  assert.equal(h.states.length, 0);
  h.sockets[1].state(2);
  old.onclose();
  assert.equal(h.live.ready, true);
  assert.equal(h.states.length, 1);
  h.live.stop();
});

test('healthy sockets survive foreground events, stale sockets recover after sleep', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  h.sockets[0].state();
  h.advance(1000);
  h.live.recover();
  assert.equal(h.sockets.length, 1);
  h.suspend(21000);
  h.live.recover();
  h.live.recover();
  assert.equal(h.sockets.length, 2);
  h.live.stop();
});

test('overdue pending socket is replaced on foreground recovery', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  h.suspend(3000);
  h.live.recover();
  h.live.recover();
  assert.equal(h.sockets.length, 2);
  h.live.stop();
});

test('offline cancels work without losing credentials; online opens once', () => {
  const h = harness();
  h.live.start('token', 'ABCDEFGH');
  h.env.navigator.onLine = false;
  h.live.offline();
  h.advance(30000);
  assert.equal(h.sockets.length, 1);
  assert.equal(h.live.token, 'token');
  h.env.navigator.onLine = true;
  h.live.recover();
  h.live.recover();
  assert.equal(h.sockets.length, 2);
  h.live.stop();
});

test('session expiry clears connection work; temporary errors retain credentials', async () => {
  for (const [status, expired] of [
    [401, true],
    [503, false],
  ]) {
    const h = harness();
    h.env.fetch = async () => ({ status });
    h.live.start('token', 'ABCDEFGH');
    h.sockets[0].onerror();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(h.expired(), expired ? 1 : 0);
    assert.equal(h.live.token, expired ? '' : 'token');
    h.live.stop();
    assert.equal(h.timers.size, 0);
  }
});

test('an old diagnostic cannot expire a newly joined session', async () => {
  const h = harness();
  let finish;
  h.env.fetch = () =>
    new Promise((resolve) => {
      finish = resolve;
    });
  h.live.start('old-token', 'ABCDEFGH');
  h.sockets[0].onerror();
  h.live.start('new-token', 'ABCDEFGH');
  h.sockets[1].state();
  finish({ status: 401 });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.expired(), 0);
  assert.equal(h.live.ready, true);
  assert.equal(h.live.token, 'new-token');
  h.live.stop();
});

test('quiz validation rejects invalid scoring indices and timers', () => {
  const quiz = {
    version: 1,
    title: 'Quiz',
    questions: [
      { text: 'Question?', choices: ['A', 'B'], correctIndex: 0, timeLimitSeconds: 20 },
    ],
  };
  assert.equal(validQuiz(quiz), true);
  quiz.questions[0].correctIndex = 2;
  assert.equal(validQuiz(quiz), false);
  quiz.questions[0].correctIndex = 0;
  quiz.questions[0].timeLimitSeconds = 4;
  assert.equal(validQuiz(quiz), false);
});

test('view transitions keep the latest navigation and bypass motion when requested', async (t) => {
  const previousDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
  const previousMatchMedia = Object.getOwnPropertyDescriptor(globalThis, 'matchMedia');
  t.after(() => {
    if (previousDocument) Object.defineProperty(globalThis, 'document', previousDocument);
    else delete globalThis.document;
    if (previousMatchMedia)
      Object.defineProperty(globalThis, 'matchMedia', previousMatchMedia);
    else delete globalThis.matchMedia;
  });
  const transitions = [];
  let reducedMotion = false;
  globalThis.matchMedia = () => ({ matches: reducedMotion });
  globalThis.document = {
    hidden: false,
    startViewTransition(update) {
      const transition = {
        update,
        skipped: false,
        ready: Promise.resolve(),
        skipTransition() {
          this.skipped = true;
        },
      };
      transitions.push(transition);
      return transition;
    },
  };
  const ui = quizApp();
  let renders = 0;
  ui.$nextTick = async () => renders++;
  ui.setView('login');
  ui.setView('editor');
  assert.equal(transitions[0].skipped, true);
  await transitions[1].update();
  await transitions[0].update();
  assert.equal(ui.view, 'editor');
  assert.equal(renders, 1);

  ui.setView('history');
  reducedMotion = true;
  ui.setView('home');
  await transitions[2].update();
  assert.equal(ui.view, 'home');
  assert.equal(transitions.length, 3);

  reducedMotion = false;
  document.hidden = true;
  ui.setView('room');
  assert.equal(ui.view, 'room');
  assert.equal(transitions.length, 3);

  document.hidden = false;
  delete document.startViewTransition;
  ui.setView('library');
  assert.equal(ui.view, 'library');
});

test('new questions start with two choices and preserve the correct answer when removing extras', () => {
  const ui = quizApp();
  ui.newQuiz();
  const q = ui.draft.questions[0];
  assert.deepEqual(q.choices, ['', '']);
  ui.draft.title = 'Two choices';
  q.text = 'Pick a letter';
  q.choices = ['A', 'B'];
  assert.equal(validQuiz(ui.draft), true);
  ui.removeChoice(q, 0);
  assert.deepEqual(q.choices, ['A', 'B']);
  q.choices.push('C');
  q.correctIndex = 2;
  assert.equal(validQuiz(ui.draft), true);
  ui.removeChoice(q, 0);
  assert.deepEqual(q.choices, ['B', 'C']);
  assert.equal(q.correctIndex, 1);
  assert.equal(validQuiz(ui.draft), true);
});

test('editing a reactive quiz makes an independent draft without changing the library', () => {
  const original = {
    id: 'quiz-1',
    version: 1,
    title: 'Original quiz',
    questions: [
      {
        text: 'Original question?',
        choices: ['Yes', 'No'],
        correctIndex: 0,
        timeLimitSeconds: 20,
      },
    ],
  };
  const reactive = new Proxy(
    { ...original, questions: new Proxy([new Proxy(original.questions[0], {})], {}) },
    {},
  );
  const ui = quizApp();
  ui.editQuiz(reactive);
  assert.equal(ui.view, 'editor');
  assert.equal(ui.draft.id, 'quiz-1');
  assert.equal(ui.dirty(), false);
  ui.draft.title = 'Edited quiz';
  ui.draft.questions[0].text = 'Edited question?';
  ui.draft.questions[0].choices[0] = 'Maybe';
  assert.equal(ui.dirty(), true);
  assert.equal(original.title, 'Original quiz');
  assert.equal(original.questions[0].text, 'Original question?');
  assert.deepEqual(original.questions[0].choices, ['Yes', 'No']);
});

test('English and Turkish provide the same UI messages', () => {
  assert.deepEqual(Object.keys(messages.en).sort(), Object.keys(messages.tr).sort());
});

test('AI prompts contain a valid import example in both languages', () => {
  for (const language of ['en', 'tr']) {
    const prompt = quizPrompt(language);
    const example = JSON.parse(prompt.slice(prompt.indexOf('{')));
    assert.equal(validQuiz(example), true);
    assert.match(prompt, /correctIndex/);
    assert.match(prompt, /5[–-]120|5 to 120/);
  }
});

test('file selection and file paste populate the JSON editor without importing', async () => {
  const ui = quizApp();
  let requests = 0;
  ui.request = async () => {
    requests++;
  };
  const text = '{"version":1,"title":"From a file"}';
  const file = new File([text], 'quiz.json', { type: 'application/json' });
  await ui.loadImportFile(file);
  assert.equal(ui.importText, text);
  assert.equal(requests, 0);
  let prevented = false;
  ui.pasteImport({
    clipboardData: { files: [file] },
    preventDefault() {
      prevented = true;
    },
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(prevented, true);
  assert.equal(ui.importText, text);
  assert.equal(requests, 0);
});

test('invalid JSON stays in the dialog; successful imports close it once', async () => {
  const ui = quizApp();
  ui.hostToken = 'host-session';
  let closed = 0;
  let requests = 0;
  ui.$refs = {
    importDialog: {
      close() {
        closed++;
      },
    },
  };
  ui.request = async (path, options) => {
    requests++;
    assert.equal(path, '/quizzes/import');
    return { ...options.body, id: 'saved' };
  };
  ui.importText = '{broken';
  await ui.importQuiz();
  assert.equal(requests, 0);
  assert.equal(closed, 0);
  assert.ok(ui.importError);
  assert.equal(ui.importText, '{broken');
  ui.importText = '{"version":1,"title":"Quiz","questions":[]}';
  await ui.importQuiz();
  assert.equal(requests, 1);
  assert.equal(closed, 1);
  assert.equal(ui.quizzes[0].id, 'saved');
  assert.equal(ui.busy, false);
});
