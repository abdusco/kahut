import { LiveConnection } from './live.js';
import { resolveRoute, roomPath, editorPath, gamePath } from './routes.js';
import { messages, translateError } from './i18n.js';

const storage = {
  get(key, fallback = '') {
    try {
      return localStorage.getItem(`kahut.${key}`) || fallback;
    } catch {
      return fallback;
    }
  },
  set(key, value) {
    try {
      localStorage.setItem(`kahut.${key}`, value);
    } catch {
      /* Private browsing may block storage. */
    }
  },
  remove(key) {
    try {
      localStorage.removeItem(`kahut.${key}`);
    } catch {
      /* Storage is optional. */
    }
  },
  saved() {
    try {
      return JSON.parse(this.get('session', 'null'));
    } catch {
      return null;
    }
  },
};

const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const MAX_IMAGE_BYTES = 4 * 1024 * 1024;

export function validQuiz(quiz) {
  return (
    quiz?.version === 1 &&
    typeof quiz.title === 'string' &&
    quiz.title.trim().length > 0 &&
    [...quiz.title.trim()].length <= 120 &&
    Array.isArray(quiz.questions) &&
    quiz.questions.length >= 1 &&
    quiz.questions.length <= 100 &&
    quiz.questions.every(
      (q) =>
        typeof q.text === 'string' &&
        q.text.trim() &&
        [...q.text.trim()].length <= 1000 &&
        Array.isArray(q.choices) &&
        q.choices.length >= 2 &&
        q.choices.length <= 4 &&
        q.choices.every(
          (c) => typeof c === 'string' && c.trim() && [...c.trim()].length <= 300,
        ) &&
        Number.isInteger(q.correctIndex) &&
        q.correctIndex >= 0 &&
        q.correctIndex < q.choices.length &&
        Number.isInteger(q.timeLimitSeconds) &&
        q.timeLimitSeconds >= 5 &&
        q.timeLimitSeconds <= 120,
    )
  );
}

export function quizApp() {
  // Keep sockets, native timers, and AbortControllers outside Alpine's reactive state.
  let live;
  let clock;
  let viewTransition;
  let viewChange = 0;
  return {
    language: storage.get(
      'language',
      navigator.language.toLowerCase().startsWith('tr') ? 'tr' : 'en',
    ),
    theme: storage.get('theme', 'system'),
    view: 'home',
    code: '',
    nickname: storage.get('nickname'),
    password: '',
    username: storage.get('username'),
    hostToken: storage.get('hostToken'),
    token: '',
    isHost: false,
    state: null,
    ready: false,
    busy: false,
    online: navigator.onLine,
    error: '',
    message: '',
    importText: '',
    importError: '',
    promptCopyStatus: '',
    quizzes: [],
    games: [],
    draft: null,
    draftOriginal: '',
    historyGame: null,
    savedRoom: storage.saved()?.room || '',
    now: Date.now(),
    serverOffset: 0,
    installPrompt: null,
    menuOpen: false,
    get remaining() {
      return this.state
        ? Math.max(
            0,
            Math.ceil((this.state.deadline - this.now - this.serverOffset) / 1000),
          )
        : 0;
    },
    get timePercent() {
      return Math.min(
        100,
        Math.max(
          0,
          (this.remaining / (this.state?.question?.timeLimitSeconds || 20)) * 100,
        ),
      );
    },
    get inviteURL() {
      return `${location.origin}${roomPath(this.code)}`;
    },
    get answeredCount() {
      return this.state?.answeredCount || 0;
    },
    get eligibleCount() {
      return this.state?.eligibleCount || 0;
    },
    get myStanding() {
      return this.state?.players.find((player) => player.id === this.state.playerId);
    },
    get aiPrompt() {
      return quizPrompt(this.language);
    },
    t(key) {
      return (messages[this.language] || messages.en)[key] || messages.en[key] || key;
    },
    init() {
      if (!messages[this.language]) this.language = 'en';
      this.savePreferences();
      live = new LiveConnection({
        onState: (state) => {
          if (this.state && state.sequence <= this.state.sequence) return;
          this.serverOffset = state.serverTime - Date.now();
          this.state = state;
          this.now = Date.now();
          if (state.phase === 'lobby') this.$nextTick(() => this.drawQR());
        },
        onStatus: (ready) => {
          this.ready = ready;
        },
        onExpired: () => this.expire(),
      });
      clock = setInterval(() => {
        this.now = Date.now();
      }, 100);
      const recover = () => live.recover();
      window.addEventListener('focus', recover);
      window.addEventListener('resume', recover);
      window.addEventListener('pageshow', (event) => {
        if (event.persisted) recover();
      });
      document.addEventListener('visibilitychange', () => {
        if (!document.hidden) recover();
      });
      window.addEventListener('online', () => {
        this.online = true;
        recover();
      });
      window.addEventListener('offline', () => {
        this.online = false;
        live.offline();
      });
      window.addEventListener('beforeinstallprompt', (event) => {
        event.preventDefault();
        this.installPrompt = event;
      });
      window.addEventListener('appinstalled', () => {
        this.installPrompt = null;
      });
      window.addEventListener('popstate', () => this.launch());
      window.addEventListener('beforeunload', (event) => {
        if (this.dirty()) {
          event.preventDefault();
          event.returnValue = '';
        }
      });
      this.launch();
      if ('serviceWorker' in navigator)
        navigator.serviceWorker.register('/sw.js').catch(() => {});
    },
    destroy() {
      viewChange++;
      viewTransition?.skipTransition();
      live.stop();
      clearInterval(clock);
    },
    savePreferences() {
      storage.set('language', this.language);
      storage.set('theme', this.theme);
      document.documentElement.lang = this.language;
      document.documentElement.dataset.theme = this.theme;
    },
    setView(view) {
      const change = ++viewChange;
      viewTransition?.skipTransition();
      if (
        this.view === view ||
        typeof document === 'undefined' ||
        !document.startViewTransition ||
        !this.$nextTick ||
        document.hidden ||
        matchMedia('(prefers-reduced-motion: reduce)').matches
      ) {
        this.view = view;
        return;
      }
      // A newer navigation can arrive while the browser captures the old page.
      // Only its callback may change the view; sockets and timers keep running.
      viewTransition = document.startViewTransition(async () => {
        if (change !== viewChange) return;
        this.view = view;
        // Route sections use :hidden so Alpine's DOM flush is sufficient.
        // x-show defers visibility to a frame after the snapshot is captured.
        await this.$nextTick();
      });
      viewTransition.ready.catch(() => {
        // Hidden tabs and interrupted transitions may skip their animation.
      });
    },
    launch() {
      live.stop();
      this.state = null;
      this.token = '';
      this.isHost = false;
      this.setView('home');
      const route = resolveRoute(location.pathname, location.search);
      this.navigate(route.path, true);
      if (['host', 'newQuiz', 'editQuiz', 'history'].includes(route.kind)) {
        this.run(() => this.loadHostRoute(route));
        return;
      }
      const saved = storage.saved();
      this.code = route.kind === 'room' ? route.code : '';
      if (
        saved &&
        (route.kind === 'resume' ||
          (route.kind === 'room' && route.code === saved.room?.toUpperCase()))
      ) {
        if (saved.role === 'host' && this.hostToken)
          this.enterRoom(saved.room, true, '', true);
        else if (saved.role === 'player' && saved.token)
          this.enterRoom(saved.room, false, saved.token, true);
        else if (route.kind === 'resume') this.home();
      } else if (route.kind === 'resume') this.home();
      else if (route.kind === 'notFound') {
        this.setView('loading');
        this.error = this.t('pageNotFound');
      }
    },
    navigate(path, replace = false) {
      if (typeof window === 'undefined') return;
      if (location.pathname + location.search === path) return;
      history[replace ? 'replaceState' : 'pushState'](null, '', path);
    },
    async loadHostRoute(route) {
      if (!this.hostToken) {
        this.setView('login');
        return;
      }
      this.setView('loading');
      if (route.kind === 'editQuiz') {
        const quiz = await this.request(`/quizzes/${encodeURIComponent(route.quizId)}`);
        this.setDraft(quiz, true);
      } else if (route.kind === 'newQuiz') this.newQuiz(true);
      else if (route.kind === 'history') await this.loadHistory(route.code);
      else await this.loadLibrary();
    },
    dirty() {
      return this.view === 'editor' && JSON.stringify(this.draft) !== this.draftOriginal;
    },
    home() {
      if (this.dirty() && !confirm(this.t('confirmDiscard'))) return;
      live.stop();
      this.state = null;
      this.token = '';
      this.isHost = false;
      this.setView('home');
      this.code = '';
      this.error = '';
      this.message = '';
      this.savedRoom = storage.saved()?.room || '';
      this.navigate('/', true);
    },
    resumeSaved() {
      this.navigate('/resume');
      this.launch();
    },
    expire() {
      if (this.isHost) {
        storage.remove('hostToken');
        this.hostToken = '';
      }
      storage.remove('session');
      this.savedRoom = '';
      this.home();
      this.error = this.t('expired');
    },
    async request(path, { method = 'GET', body, raw, token = this.hostToken } = {}) {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), raw ? 30000 : 10000);
      try {
        const response = await fetch(`/api${path}`, {
          method,
          headers: {
            ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
            ...(raw ? { 'Content-Type': raw.type } : {}),
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          body: raw ?? (body !== undefined ? JSON.stringify(body) : undefined),
          signal: controller.signal,
          cache: 'no-store',
        });
        const data = await response.json();
        if (!response.ok) {
          if (response.status === 401 && token && token === this.token) this.expire();
          else if (
            response.status === 401 &&
            token === this.hostToken &&
            path !== '/host/login'
          ) {
            this.hostToken = '';
            storage.remove('hostToken');
            this.setView('login');
          }
          throw new Error(
            translateError(data.error || `HTTP ${response.status}`, this.language),
          );
        }
        return data;
      } catch (error) {
        if (error instanceof TypeError || error.name === 'AbortError')
          throw new Error(this.t('networkError'));
        throw error;
      } finally {
        clearTimeout(timeout);
      }
    },
    async run(task) {
      if (this.busy) return;
      this.busy = true;
      this.error = '';
      this.message = '';
      try {
        await task();
      } catch (error) {
        this.error = error.message;
      } finally {
        this.busy = false;
      }
    },
    join() {
      return this.run(async () => {
        const result = await this.request('/join', {
          method: 'POST',
          body: { room: this.code.trim().toUpperCase(), name: this.nickname.trim() },
          token: '',
        });
        storage.set('nickname', this.nickname.trim());
        this.enterRoom(result.room, false, result.token);
      });
    },
    enterRoom(room, host, token = '', replace = false) {
      live.stop();
      this.code = room.toUpperCase();
      this.isHost = host;
      this.token = host ? this.hostToken : token;
      this.state = null;
      this.setView('room');
      this.error = '';
      this.message = '';
      storage.set(
        'session',
        JSON.stringify({
          room: this.code,
          role: host ? 'host' : 'player',
          ...(host ? {} : { token }),
        }),
      );
      this.savedRoom = this.code;
      this.navigate(roomPath(this.code), replace);
      live.start(this.token, this.code);
    },
    openHost() {
      this.navigate('/host');
      if (this.hostToken) this.run(() => this.loadHostRoute({ kind: 'host' }));
      else {
        this.setView('login');
        this.error = '';
        this.message = '';
      }
    },
    login() {
      return this.run(async () => {
        const result = await this.request('/host/login', {
          method: 'POST',
          body: { username: this.username, password: this.password },
          token: '',
        });
        this.password = '';
        this.hostToken = result.token;
        storage.set('username', this.username);
        storage.set('hostToken', result.token);
        await this.loadHostRoute(resolveRoute(location.pathname, location.search));
      });
    },
    logout() {
      return this.run(async () => {
        await this.request('/host/logout', { method: 'POST', body: {} });
        this.hostToken = '';
        this.quizzes = [];
        this.games = [];
        this.draft = null;
        this.draftOriginal = '';
        this.historyGame = null;
        storage.remove('hostToken');
        if (storage.saved()?.role === 'host') storage.remove('session');
        this.home();
      });
    },
    async loadLibrary() {
      const [quizzes, games] = await Promise.all([
        this.request('/quizzes'),
        this.request('/games'),
      ]);
      this.quizzes = quizzes;
      this.games = games;
      this.setView('library');
      this.navigate('/host');
    },
    backToLibrary() {
      if (this.dirty() && !confirm(this.t('confirmDiscard'))) return;
      live.stop();
      this.token = '';
      this.state = null;
      this.isHost = false;
      this.navigate('/host');
      return this.run(() => this.loadLibrary());
    },
    blankQuestion() {
      return {
        text: '',
        choices: ['', ''],
        correctIndex: 0,
        timeLimitSeconds: 20,
      };
    },
    setDraft(quiz, replace = false) {
      // Quiz documents are JSON. Serialization also unwraps Alpine's proxies,
      // which structuredClone cannot copy.
      this.draft = JSON.parse(JSON.stringify(quiz));
      this.draftOriginal = JSON.stringify(this.draft);
      this.setView('editor');
      this.error = '';
      this.message = '';
      this.navigate(editorPath(quiz.id), replace);
    },
    newQuiz(replace = false) {
      this.setDraft(
        { version: 1, title: '', questions: [this.blankQuestion()] },
        replace,
      );
    },
    editQuiz(quiz) {
      this.setDraft(quiz);
    },
    sampleQuiz() {
      this.setDraft({
        version: 1,
        title: this.t('sampleTitle'),
        questions: [
          {
            text: this.t('sampleQ1'),
            choices: [
              this.t('mars'),
              this.t('venus'),
              this.t('jupiter'),
              this.t('saturn'),
            ],
            correctIndex: 0,
            timeLimitSeconds: 20,
          },
          {
            text: this.t('sampleQ2'),
            choices: ['5', '6', '7', '8'],
            correctIndex: 1,
            timeLimitSeconds: 15,
          },
          {
            text: this.t('sampleQ3'),
            choices: ['Ag', 'Fe', 'Au', 'Cu'],
            correctIndex: 2,
            timeLimitSeconds: 20,
          },
        ],
      });
    },
    moveQuestion(index, delta) {
      const questions = this.draft.questions;
      const [q] = questions.splice(index, 1);
      questions.splice(index + delta, 0, q);
    },
    imageUrl(id) {
      return `/api/images/${encodeURIComponent(id)}`;
    },
    // Uploads immediately so the editor can preview the image. Unsaved uploads
    // that no quiz uses are removed by the server after a day.
    addImage(q, file) {
      if (!file || this.busy) return;
      if (!IMAGE_TYPES.includes(file.type)) {
        this.error = this.t('imageType');
        return;
      }
      if (file.size > MAX_IMAGE_BYTES) {
        this.error = this.t('imageTooLarge');
        return;
      }
      return this.run(async () => {
        q.imageId = (await this.request('/images', { method: 'POST', raw: file })).id;
      });
    },
    pasteImage(event, q) {
      const file = [...(event.clipboardData?.files ?? [])].find((f) =>
        f.type.startsWith('image/'),
      );
      if (file) {
        event.preventDefault();
        this.addImage(q, file);
      }
    },
    dropImage(event, q) {
      this.addImage(q, event.dataTransfer.files[0]);
    },
    removeChoice(q, index) {
      if (q.choices.length <= 2) return;
      q.choices.splice(index, 1);
      if (q.correctIndex === index) q.correctIndex = 0;
      else if (q.correctIndex > index) q.correctIndex--;
    },
    saveQuiz() {
      return this.run(async () => {
        if (!validQuiz(this.draft)) throw new Error(this.t('invalidQuiz'));
        await this.request(this.draft.id ? `/quizzes/${this.draft.id}` : '/quizzes', {
          method: this.draft.id ? 'PUT' : 'POST',
          body: this.draft,
        });
        this.draftOriginal = JSON.stringify(this.draft);
        await this.loadLibrary();
        this.message = this.t('saved');
      });
    },
    openSettings() {
      this.$refs.settingsDialog.showModal();
      this.menuOpen = true;
    },
    openImport() {
      this.importText = '';
      this.importError = '';
      this.promptCopyStatus = '';
      this.$refs.importDialog.showModal();
    },
    async loadImportFile(file) {
      if (!file || this.busy) return;
      this.importError = '';
      try {
        if (file.size > 1048576) throw new Error(this.t('importError'));
        this.importText = await file.text();
      } catch {
        this.importError = this.t('importError');
      }
    },
    pasteImport(event) {
      const file = event.clipboardData?.files[0];
      if (file) {
        event.preventDefault();
        this.loadImportFile(file);
      }
    },
    async copyPrompt() {
      try {
        await navigator.clipboard.writeText(this.aiPrompt);
        this.promptCopyStatus = this.t('promptCopied');
      } catch {
        this.$refs.aiPrompt.focus();
        this.$refs.aiPrompt.select();
        this.promptCopyStatus = this.t('copyManually');
      }
    },
    async importQuiz() {
      if (this.busy) return;
      this.busy = true;
      this.importError = '';
      try {
        let quiz;
        try {
          if (new TextEncoder().encode(this.importText).length > 1048576)
            throw new Error();
          quiz = JSON.parse(this.importText);
        } catch {
          throw new Error(this.t('importError'));
        }
        const saved = await this.request('/quizzes/import', {
          method: 'POST',
          body: quiz,
        });
        this.quizzes.unshift(saved);
        this.$refs.importDialog.close();
        this.error = '';
        this.message = this.t('imported');
      } catch (error) {
        this.importError = error.message;
        if (!this.hostToken) this.$refs.importDialog.close();
      } finally {
        this.busy = false;
      }
    },
    exportQuiz(quiz) {
      return this.run(async () => {
        const data = await this.request(`/quizzes/${quiz.id}/export`);
        const blob = new Blob([JSON.stringify(data, null, 2)], {
          type: 'application/json',
        });
        const url = URL.createObjectURL(blob);
        const anchor = document.createElement('a');
        anchor.href = url;
        anchor.download = 'quiz.json';
        anchor.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      });
    },
    deleteQuiz(quiz) {
      if (!confirm(this.t('confirmDelete'))) return;
      return this.run(async () => {
        await this.request(`/quizzes/${quiz.id}`, { method: 'DELETE' });
        await this.loadLibrary();
      });
    },
    hostQuiz(id) {
      return this.run(async () => {
        const result = await this.request('/rooms', {
          method: 'POST',
          body: { quizId: id },
        });
        this.enterRoom(result.room, true);
      });
    },
    act(action, extra = {}) {
      if (!this.ready || !this.state) return;
      return this.run(() =>
        this.request('/action', {
          method: 'POST',
          token: this.token,
          body: { room: this.code, action, revision: this.state.revision, ...extra },
        }),
      );
    },
    answer(choice) {
      if (!this.state || this.state.answered || !this.state.eligible) return;
      return this.act('answer', { question: this.state.questionIndex, choice });
    },
    finishGame() {
      if (confirm(this.t('confirmEnd'))) return this.act('finish');
    },
    showHistory(code) {
      return this.run(() => this.loadHistory(code));
    },
    async loadHistory(code) {
      this.historyGame = await this.request(`/games/${code}`);
      this.setView('history');
      this.navigate(gamePath(code));
    },
    historicalAnswer(player, question, q) {
      const answer = this.historyGame.game.answers.find(
        (a) => a.playerId === player && a.question === question,
      );
      return answer
        ? `${q.choices[answer.choice]} · ${answer.points} ${this.t('points')}`
        : this.t('unanswered');
    },
    drawQR() {
      const canvas = this.$refs.qrCanvas;
      if (!canvas || !globalThis.qrcode) return;
      const qr = globalThis.qrcode(0, 'M');
      qr.addData(this.inviteURL);
      qr.make();
      const cells = qr.getModuleCount();
      const cell = 5;
      const margin = 4;
      canvas.width = canvas.height = (cells + margin * 2) * cell;
      const ctx = canvas.getContext('2d');
      ctx.fillStyle = '#fff';
      ctx.fillRect(0, 0, canvas.width, canvas.height);
      ctx.fillStyle = '#211b36';
      for (let row = 0; row < cells; row++)
        for (let col = 0; col < cells; col++)
          if (qr.isDark(row, col))
            ctx.fillRect((col + margin) * cell, (row + margin) * cell, cell, cell);
    },
    async copyInvite() {
      try {
        await navigator.clipboard.writeText(this.inviteURL);
        this.message = this.t('copied');
      } catch {
        this.$refs.qrCanvas?.parentElement.querySelector('input')?.select();
      }
    },
    async install() {
      const prompt = this.installPrompt;
      if (!prompt) return;
      await prompt.prompt();
      this.installPrompt = null;
    },
  };
}

export function quizPrompt(language) {
  const instructions =
    language === 'tr'
      ? '[KONUNUZ] hakkında Türkçe 10 soruluk bir bilgi yarışması oluştur. Yalnızca ham JSON döndür; Markdown veya açıklama ekleme. Her soruda 2–4 seçenek ve yalnızca bir doğru cevap olsun. correctIndex sıfırdan başlar. timeLimitSeconds 5–120 arasında bir tam sayı olsun (varsayılan 20). version: 1 kullan, id ekleme. Aşağıdaki biçimi kullan ve örnek soruyu kendi sorularınla değiştir:'
      : 'Create a 10-question quiz in English about [YOUR TOPIC]. Return only raw JSON, without Markdown or commentary. Each question must have 2–4 choices and exactly one correct answer. correctIndex is zero-based. timeLimitSeconds must be an integer from 5 to 120 (default 20). Use version: 1 and omit IDs. Follow this format, replacing the example with your own questions:';
  return (
    instructions +
    '\n\n' +
    JSON.stringify(
      {
        version: 1,
        title: language === 'tr' ? 'Yarışma başlığı' : 'Quiz title',
        questions: [
          {
            text:
              language === 'tr'
                ? 'Fransa’nın başkenti neresidir?'
                : 'What is the capital of France?',
            choices: ['Paris', 'Berlin', 'Madrid', 'Rome'],
            correctIndex: 0,
            timeLimitSeconds: 20,
          },
        ],
      },
      null,
      2,
    )
  );
}

// Loading Alpine after registering the factory avoids module/defer timing races.
if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  window.quizApp = quizApp;
  const script = document.createElement('script');
  script.src = '/vendor/alpine-3.15.0.js';
  document.head.appendChild(script);
}
