// Apply appearance before CSS paints, including when storage is unavailable.
try {
  const preference = localStorage.getItem('kahut.theme') || 'system';
  document.documentElement.dataset.theme = preference;
} catch {
  document.documentElement.dataset.theme = 'system';
}
