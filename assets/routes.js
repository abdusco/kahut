export const roomPath = (code) => `/rooms/${encodeURIComponent(code.toUpperCase())}`;
export const editorPath = (id) =>
  id ? `/host/quizzes/${encodeURIComponent(id)}/edit` : '/host/quizzes/new';
export const gamePath = (code) => `/host/games/${encodeURIComponent(code.toUpperCase())}`;

// This also recognizes old invitations and installed-app URLs, then replaces
// them with canonical paths. New links never use query parameters for routing.
export function resolveRoute(pathname, search = '') {
  const path = pathname.length > 1 ? pathname.replace(/\/$/, '') : pathname;
  if (path === '/') {
    const params = new URLSearchParams(search);
    const code = params.get('room')?.trim().toUpperCase();
    if (code) return { kind: 'room', code, path: roomPath(code) };
    if (params.get('resume') === '1') return { kind: 'resume', path: '/resume' };
    return { kind: 'home', path: '/' };
  }
  if (path === '/host') return { kind: 'host', path };
  if (path === '/host/quizzes/new') return { kind: 'newQuiz', path };
  if (path === '/resume') return { kind: 'resume', path };
  let match = path.match(/^\/host\/quizzes\/([A-Za-z0-9_-]+)\/edit$/);
  if (match) return { kind: 'editQuiz', quizId: match[1], path };
  match = path.match(/^\/host\/games\/([A-Za-z0-9]{8})$/);
  if (match)
    return { kind: 'history', code: match[1].toUpperCase(), path: gamePath(match[1]) };
  match = path.match(/^\/rooms\/([A-Za-z0-9]{8})$/);
  if (match)
    return { kind: 'room', code: match[1].toUpperCase(), path: roomPath(match[1]) };
  return { kind: 'notFound', path };
}
