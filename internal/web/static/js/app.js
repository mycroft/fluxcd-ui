// Small client-side behaviors; everything else is driven by htmx attributes.
(() => {
  const closeDrawer = () => document.getElementById('drawer').replaceChildren();

  document.addEventListener('click', (e) => {
    if (e.target.closest('[data-close-drawer]')) closeDrawer();
    if (e.target.closest('[data-theme-toggle]')) toggleTheme();
    e.target.closest('[data-toast]')?.remove();
  });

  // The theme follows the OS until toggled; the choice is kept in a cookie so
  // the server renders it (<html data-theme>) on the next page load.
  const toggleTheme = () => {
    const root = document.documentElement;
    const current = root.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    const next = current === 'dark' ? 'light' : 'dark';
    root.dataset.theme = next;
    const secure = location.protocol === 'https:' ? '; Secure' : '';
    document.cookie = `theme=${next}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
  };

  document.addEventListener('keydown', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.key === 'Escape') {
      closeDrawer();
    } else if (e.key === '/' && !e.target.closest('input, select, textarea')) {
      e.preventDefault();
      document.getElementById('q').focus();
    } else if (e.key === 'Enter' && e.target.matches('tr[hx-get]')) {
      e.target.click();
    }
  });

  // Swap 404 responses into the drawer, so a deleted object shows as such,
  // refusals (401, 403), which explain themselves, and action errors, which
  // come with a toast explaining them.
  document.addEventListener('htmx:beforeSwap', (e) => {
    const { xhr, target, requestConfig } = e.detail;
    if (([401, 403, 404].includes(xhr.status) && target.closest('#drawer')) || requestConfig?.elt?.closest('[data-action]')) {
      e.detail.shouldSwap = true;
      e.detail.isError = false;
    }
  });

  // Toasts disappear on their own; errors stay longer.
  document.addEventListener('htmx:oobAfterSwap', () => {
    document.querySelectorAll('[data-toast]:not([data-expires])').forEach((el) => {
      el.dataset.expires = 'true';
      setTimeout(() => el.remove(), el.getAttribute('role') === 'alert' ? 12000 : 5000);
    });
  });

  // Keep relative times current between server updates. Same format as
  // ago() in funcs.go.
  const ago = (ms) => {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm';
    if (s < 48 * 3600) return Math.floor(s / 3600) + 'h';
    return Math.floor(s / 86400) + 'd';
  };
  setInterval(() => {
    const now = Date.now();
    document.querySelectorAll('time[datetime]').forEach((el) => {
      const t = Date.parse(el.getAttribute('datetime'));
      if (!Number.isNaN(t)) el.textContent = ago(now - t);
    });
  }, 15000);

  // Live indicator. Events may have been missed while disconnected, so
  // refresh everything after a reconnect.
  let disconnected = false;
  const setLive = (state, label) => {
    const el = document.getElementById('live');
    el.dataset.live = state;
    el.querySelector('[data-label]').textContent = label;
  };
  document.addEventListener('htmx:sseOpen', () => {
    setLive('open', 'Live');
    if (disconnected) {
      htmx.ajax('GET', '/fragments/content' + location.search, '#content');
      disconnected = false;
    }
  });
  document.addEventListener('htmx:sseError', () => {
    setLive('closed', 'Reconnecting…');
    disconnected = true;
  });
})();
