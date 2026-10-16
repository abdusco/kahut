// This module owns the only socket, retry timer, and first-state deadline.
// HTTP commands are never queued or replayed when connectivity returns.
export class LiveConnection {
  constructor({ onState, onStatus, onExpired, env = globalThis }) {
    this.env = env;
    this.onState = onState;
    this.onStatus = onStatus;
    this.onExpired = onExpired;
    this.generation = 0;
    this.backoff = 250;
    this.token = '';
    this.socket = null;
    this.retry = null;
    this.deadline = null;
    this.diagnostic = null;
    this.lastMessage = 0;
    this.connectStarted = 0;
    this.ready = false;
  }

  start(token, room) {
    this.stop();
    this.token = token;
    this.room = room;
    this.backoff = 250;
    this.connect();
    this.watch = this.env.setInterval(() => {
      if (!this.env.document.hidden) this.recover(false);
    }, 5000);
  }

  stop() {
    this.token = '';
    this.invalidate();
    this.env.clearInterval(this.watch);
    this.watch = null;
    this.cancelDiagnostic();
  }

  invalidate() {
    this.generation++;
    this.env.clearTimeout(this.retry);
    this.env.clearTimeout(this.deadline);
    this.retry = null;
    this.deadline = null;
    const old = this.socket;
    this.socket = null;
    this.ready = false;
    this.onStatus(false);
    if (old) old.close();
  }

  connect() {
    if (!this.token || this.env.navigator.onLine === false) return;
    const generation = ++this.generation;
    this.connectStarted = this.env.Date.now();
    this.lastMessage = 0;
    const url = new URL('/api/live', this.env.location.href);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('token', this.token);
    url.searchParams.set('room', this.room);
    let socket;
    try {
      socket = new this.env.WebSocket(url.href);
    } catch {
      this.fail(generation);
      return;
    }
    this.socket = socket;
    this.deadline = this.env.setTimeout(() => this.fail(generation), 2000);
    socket.onmessage = (event) => {
      if (generation !== this.generation) return;
      let message;
      try {
        message = JSON.parse(event.data);
      } catch {
        return;
      }
      if (message.kind === 'heartbeat') {
        this.lastMessage = this.env.Date.now();
        return;
      }
      if (
        message.kind !== 'state' ||
        !message.snapshot ||
        message.snapshot.code !== this.room ||
        !Number.isSafeInteger(message.snapshot.sequence)
      )
        return;
      this.lastMessage = this.env.Date.now();
      // Apply state synchronously before exposing ready controls.
      this.onState(message.snapshot);
      this.ready = true;
      this.backoff = 250;
      this.env.clearTimeout(this.deadline);
      this.deadline = null;
      this.onStatus(true);
    };
    socket.onclose = () => this.fail(generation);
    socket.onerror = () => this.fail(generation);
  }

  fail(generation) {
    if (generation !== this.generation || !this.token) return;
    this.invalidate();
    if (this.env.navigator.onLine === false) return;
    this.diagnose();
    const delay = this.backoff + Math.floor(this.env.Math.random() * 251);
    this.backoff = Math.min(this.backoff * 2, 10000);
    this.retry = this.env.setTimeout(() => {
      this.retry = null;
      this.connect();
    }, delay);
  }

  cancelDiagnostic() {
    if (!this.diagnostic) return;
    this.diagnostic.controller.abort();
    this.env.clearTimeout(this.diagnostic.timeout);
    this.diagnostic = null;
  }

  async diagnose() {
    if (this.diagnostic || !this.token) return;
    const token = this.token;
    const controller = new this.env.AbortController();
    const diagnostic = {
      controller,
      timeout: this.env.setTimeout(() => controller.abort(), 3000),
    };
    this.diagnostic = diagnostic;
    try {
      const response = await this.env.fetch('/api/session', {
        headers: { Authorization: `Bearer ${token}` },
        signal: controller.signal,
        cache: 'no-store',
      });
      if (
        response.status === 401 &&
        this.token === token &&
        this.diagnostic === diagnostic
      ) {
        this.stop();
        this.onExpired();
      }
    } catch {
      /* A network failure does not revoke a saved session. */
    } finally {
      this.env.clearTimeout(diagnostic.timeout);
      if (this.diagnostic === diagnostic) this.diagnostic = null;
    }
  }

  offline() {
    this.invalidate();
    this.cancelDiagnostic();
  }

  recover(resetBackoff = true) {
    if (!this.token || this.env.navigator.onLine === false) return;
    if (this.retry && !resetBackoff) return;
    const now = this.env.Date.now();
    if (this.socket && !this.ready && now - this.connectStarted < 2000) return;
    if (
      this.socket &&
      this.ready &&
      this.socket.readyState === 1 &&
      now - this.lastMessage < 20000
    )
      return;
    this.invalidate();
    if (resetBackoff) this.backoff = 250;
    this.connect();
  }
}
