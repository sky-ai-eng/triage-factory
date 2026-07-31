/* Wizard flash spy — paste into the DevTools console on /setup, reproduce the
 * flash, then press Ctrl+Shift+Y to download wizard-trace.json. */
(() => {
  if (window.__wizspy) return console.log('wizspy: already armed');
  const t0 = performance.now();
  const now = () => Math.round((performance.now() - t0) * 10) / 10;
  const log = [];
  const push = (kind, detail = '') => {
    log.push({ t: now(), y: Math.round(window.scrollY), kind, detail });
    if (log.length > 30000) log.splice(0, 8000);
  };
  const site = () =>
    (new Error().stack || '')
      .split('\n')
      .slice(3, 6)
      .join('<-')
      .replace(/https?:\/\/[^ )]+\//g, '')
      .slice(0, 160);

  try {
    for (const n of ['scrollTo', 'scroll', 'scrollBy']) {
      const orig = window[n].bind(window);
      window[n] = (...a) => {
        const top = typeof a[0] === 'object' ? a[0]?.top : a[1];
        push(n, `top=${Math.round(top ?? -1)} @${site()}`);
        return orig(...a);
      };
    }
    for (const n of ['scrollIntoView', 'scrollIntoViewIfNeeded']) {
      const o = Element.prototype[n];
      if (o)
        Element.prototype[n] = function (...a) {
          push(n, `${this.tagName} @${site()}`);
          return o.apply(this, a);
        };
    }
    const f = HTMLElement.prototype.focus;
    HTMLElement.prototype.focus = function (o) {
      push('focus', `${this.tagName} preventScroll=${!!o?.preventScroll}`);
      return f.call(this, o);
    };
  } catch (e) {
    console.warn('wizspy: api wrap failed', e);
  }

  try {
    const rect = (r) => `y${Math.round(r.y)}h${Math.round(r.height)}`;
    new PerformanceObserver((l) => {
      for (const e of l.getEntries()) {
        const src = (e.sources || [])
          .map((s) => {
            const n = s.node;
            const tag =
              n && n.nodeType === 1
                ? `${n.tagName}.${String(n.className).slice(0, 28)}`
                : String(n?.nodeName ?? '?');
            return `${tag} ${rect(s.previousRect)}->${rect(s.currentRect)}`;
          })
          .join(' | ');
        push('layout-shift', `score=${e.value.toFixed(4)} ${src}`);
      }
    }).observe({ type: 'layout-shift', buffered: true });
  } catch (e) {
    console.warn('wizspy: layout-shift observer failed', e);
  }

  try {
    const of = window.fetch;
    window.fetch = function (...a) {
      const u = String(a[0]?.url ?? a[0]).replace(location.origin, '');
      const started = now();
      return of.apply(this, a).then(
        (r) => {
          push('fetch', `${u} ${r.status} started@${started}`);
          return r;
        },
        (err) => {
          push('fetch', `${u} FAILED started@${started}`);
          throw err;
        },
      );
    };
  } catch (e) {
    console.warn('wizspy: fetch wrap failed', e);
  }

  let last = '';
  const frame = () => {
    try {
      const lis = [...document.querySelectorAll('ol > li:not([aria-hidden])')];
      const steps = lis
        .map((li) => {
          const h3 = li.querySelector('h3[aria-current="step"]');
          const title =
            (h3 ?? li.querySelector('button span, h3'))?.textContent?.trim().slice(0, 18) ?? '?';
          return `${title}${h3 ? '*' : ''}:${Math.round(li.getBoundingClientRect().height)}`;
        })
        .join('|');
      const row = lis
        .map((li) => [...li.children].find((c) => c.className?.includes?.('justify-between')))
        .find(Boolean);
      const sig = [
        Math.round(window.scrollY),
        Math.round(document.documentElement.scrollHeight),
        steps,
        row ? Number(getComputedStyle(row).opacity).toFixed(2) : '-',
      ].join(' § ');
      if (sig !== last) {
        last = sig;
        push('frame', sig);
      }
    } catch {
      /* keep sampling */
    }
    requestAnimationFrame(frame);
  };
  requestAnimationFrame(frame);

  const dump = () => {
    const blob = new Blob(
      [
        JSON.stringify({
          ua: navigator.userAgent,
          viewport: `${innerWidth}x${innerHeight}`,
          dpr: devicePixelRatio,
          reducedMotion: matchMedia('(prefers-reduced-motion: reduce)').matches,
          log,
        }),
      ],
      { type: 'application/json' },
    );
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'wizard-trace.json';
    a.click();
    console.log(`wizspy: dumped ${log.length} entries`);
  };
  addEventListener('keydown', (e) => {
    if (e.ctrlKey && e.shiftKey && e.key.toLowerCase() === 'y') dump();
  });
  window.__wizspy = { log, dump };
  console.log('wizspy: armed — reproduce the flash, then Ctrl+Shift+Y to download the trace');
})();
