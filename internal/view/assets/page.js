// page.js draws the witness view snapshot. Every number it shows was computed in Go and arrives
// in the #data JSON model; this file only lays it out. Text reaches the page only through
// textContent, text nodes and setAttribute (render.go's bundle guard rejects every HTML-parsing
// API), and all state lives in this closure: nothing is stored, nothing touches the URL.
(function () {
  'use strict';
  const M = JSON.parse(document.getElementById('data').textContent);
  const A = M.activity;

  // ---- Helpers

  // el builds an element; `text` sets textContent, every other key is an attribute, and string
  // children become text nodes (append never parses markup).
  function el(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const k in attrs || {}) {
      if (k === 'text') e.textContent = attrs[k];
      else e.setAttribute(k, attrs[k]);
    }
    for (const c of kids) if (c != null) e.append(c);
    return e;
  }
  function svg(tag, attrs, text) {
    const e = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (text != null) e.textContent = text;
    return e;
  }

  // Dates arrive as YYYY-MM-DD, already in the archive owner's zone. Treating them as UTC
  // midnight and formatting in UTC keeps the browser's own zone from moving a day.
  const DAY = 864e5;
  const ms = s => Date.UTC(+s.slice(0, 4), +s.slice(5, 7) - 1, +s.slice(8, 10));
  const iso = t => new Date(t).toISOString().slice(0, 10);
  const fmt = (s, o) => new Date(ms(s)).toLocaleDateString('en-US', Object.assign({ timeZone: 'UTC' }, o));
  const weekday = s => fmt(s, { weekday: 'short', month: 'short', day: 'numeric' }); // Mon, Nov 2
  // "Aug 11", with the year only when it isn't the snapshot's.
  const dateText = s => fmt(s, s.slice(0, 4) === A.last.slice(0, 4)
    ? { month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric', year: 'numeric' });
  const num = n => Math.round(n).toLocaleString('en-US');
  const count = (n, word) => num(n) + ' ' + word + (n === 1 ? '' : 's');
  const calm = matchMedia('(prefers-reduced-motion: reduce)').matches;

  // ---- Pages: the header switches between them; state is in memory only.

  const state = { page: 'overview', trait: null };
  const tabs = {};
  const header = el('header', { class: 'top' }, el('span', { class: 'brand', text: 'witness' }),
    el('nav', { class: 'tabs' }, ...[['overview', 'Overview'], ['changed', "How you've changed"]].map(([p, label]) => {
      const b = el('button', { type: 'button', text: label });
      b.addEventListener('click', () => showPage(p));
      return (tabs[p] = b);
    })));
  const pages = {
    overview: el('section', { 'data-page': 'overview', class: 'page overview' }),
    changed: el('section', { 'data-page': 'changed', class: 'page changed' }),
  };
  function showPage(p) {
    state.page = p;
    for (const k in pages) {
      pages[k].hidden = k !== p;
      tabs[k].classList.toggle('on', k === p);
    }
    window.scrollTo(0, 0);
  }
  // openTrait is page 1's door into page 2 ("see how it changed →"): page 2 draws state.trait,
  // the trait it opens with, under that trait's topic.
  function openTrait(id) {
    state.trait = id;
    showPage('changed');
  }

  // ---- Activity: one window of time, its calendar, five numbers and three facts.

  const KINDS = [['month', 'Month'], ['quarter', 'Quarter'], ['half', 'Half year'], ['year', 'Year']];
  const byKind = {};
  for (const w of A.windows) (byKind[w.kind] = byKind[w.kind] || []).push(w);
  const dayMap = new Map(A.days.map(d => [d.date, d]));
  // One ramp for every window: the cuts are quantiles over ALL days, so a shade means the same
  // amount of activity whichever window shows it.
  const RAMP = ['#1f1f1e', '#184f95', '#256abf', '#3987e5', '#6da7ec', '#9ec5f4'];
  const NODATA = '#171716';
  const shade = turns => turns ? 1 + A.cuts.filter(c => c < turns).length : 0;

  // windowAt finds the window of `kind` holding `date`, else the first one after it, else the
  // last. Windows only run from the first conversation's to the snapshot's, so switching from a
  // Year to Month lands on the first month that exists rather than on an empty January.
  function windowAt(kind, date) {
    const i = byKind[kind].findIndex(w => w.end >= date);
    return i < 0 ? byKind[kind].length - 1 : i;
  }
  let kind = 'year';
  let wi = windowAt('year', A.last);
  let shown = null; // the five numbers on screen, so the next window counts from them

  const actTitle = el('h1');
  const kindBtns = KINDS.map(([k, label]) => {
    const b = el('button', { type: 'button', class: 'pbtn', text: label });
    b.addEventListener('click', () => {
      if (k === kind) return;
      const from = byKind[kind][wi].start;
      kind = k;
      wi = windowAt(k, from);
      drawActivity(true);
    });
    return b;
  });
  const prev = el('button', { type: 'button', 'aria-label': 'earlier', text: '‹' });
  const next = el('button', { type: 'button', 'aria-label': 'later', text: '›' });
  const plabel = el('span');
  prev.addEventListener('click', () => { if (wi > 0) { wi--; drawActivity(true); } });
  next.addEventListener('click', () => { if (wi < byKind[kind].length - 1) { wi++; drawActivity(true); } });

  const cal = svg('svg', { class: 'cal', 'data-calendar': '' });
  const legend = el('div', { class: 'legend' }, 'fewer turns', ...RAMP.map(c => {
    const i = el('i');
    i.style.background = c;
    return i;
  }), 'more');
  const calbox = el('div', { class: 'calbox' }, cal, legend);
  const NUMS = ['conversations', 'turns captured', 'notes taken', 'new traits', 'rewrites'];
  const nums = el('div', { class: 'nums' }, ...NUMS.map(label => el('div', { class: 'num' }, el('b'), el('span', { text: label }))));
  const facts = el('div', { class: 'facts' });
  const fine = el('p', { class: 'fine', text: count(A.totalTraits, 'trait') + ' in your archive today.' });
  const stats = el('div', { class: 'stats' });
  const tip = el('div', { class: 'tip' });

  // tween counts a number from what it showed to its new value (ease-out). Reduced motion and
  // the first draw set it outright; a newer tween on the same node stops an older one.
  function tween(node, from, to) {
    const gen = node.tween = (node.tween || 0) + 1, t0 = performance.now();
    if (calm || from == null || from === to) { node.textContent = num(to); return; }
    const step = now => {
      if (node.tween !== gen) return;
      const k = Math.min(1, (now - t0) / 550);
      node.textContent = num(from + (to - from) * (1 - Math.pow(1 - k, 3)));
      if (k < 1) requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }

  function drawActivity(animate) {
    const ws = byKind[kind], w = ws[wi];
    kindBtns.forEach((b, i) => b.classList.toggle('on', KINDS[i][0] === kind));
    plabel.textContent = w.label + (kind === 'quarter' || kind === 'half'
      ? ' · ' + fmt(w.start, { month: 'short' }) + '–' + fmt(w.end, { month: 'short' }) : '');
    prev.disabled = wi === 0;
    next.disabled = wi === ws.length - 1;
    actTitle.textContent = w.label + ': ' + (w.activeDays ? count(w.activeDays, 'day') + ' with you' : 'nothing recorded');
    drawCalendar(w, animate);

    if (!A.first) { // nothing at all: no numbers to show, in this window or any other
      stats.replaceChildren(el('p', { class: 'nothing', text: 'Nothing recorded yet.' }), fine);
      return;
    }
    const vals = [w.conv, w.turns, w.notes, w.newTraits, w.rewrites];
    [...nums.children].forEach((c, k) => tween(c.firstChild, shown && shown[k], vals[k]));
    shown = vals;
    facts.replaceChildren(
      el('div', {}, el('b', { text: num(w.activeDays) + ' of ' + num(w.spanDays) }),
        ' day' + (w.spanDays === 1 ? '' : 's') + (w.soFar ? ' so far' : '') + ' had a conversation'),
      w.longestRun ? el('div', {}, 'Longest run: ', el('b', { text: count(w.longestRun, 'day') + ' in a row' })) : null,
      w.busiest ? el('div', {}, 'Busiest day: ', el('b', { text: weekday(w.busiest.date) + ', ' + count(w.busiest.turns, 'turn') })) : null);
    if (stats.firstChild !== nums) stats.replaceChildren(nums, facts, fine);
  }

  // The calendar: weeks as columns, Monday down. Squares are sized from the box's width so a
  // month and a year both fill it (about 34 px and 10 px). Squares fade in left to right only
  // when the reader picked the window; a resize redraws them still.
  let drawnWidth = -1;
  const tips = new Map();
  function drawCalendar(w, animate) {
    animate = animate && !calm;
    drawnWidth = calbox.clientWidth;
    const a = ms(w.start), b = ms(w.end);
    const mon0 = a - ((new Date(a).getUTCDay() + 6) % 7) * DAY;
    const cols = Math.floor((b - mon0) / DAY / 7) + 1;
    const wide = cols > 30, G = wide ? 3 : 5;
    const S = Math.max(4, Math.min(34, Math.floor((drawnWidth - 34) / cols) - G));
    const W = 34 + cols * (S + G), H = 22 + 7 * (S + G);
    cal.setAttribute('width', W);
    cal.setAttribute('height', H);
    cal.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    cal.classList.toggle('anim', animate);
    const kids = [];
    ['Mon', '', 'Wed', '', 'Fri', '', ''].forEach((t, r) => {
      if (t) kids.push(svg('text', { x: 0, y: 22 + r * (S + G) + S / 2 + 4, class: 'tick' }, t));
    });
    tips.clear();
    let lastM = -1, lastW = -9;
    for (let t = a; t <= b; t += DAY) {
      const k = Math.round((t - mon0) / DAY), wk = Math.floor(k / 7), s = iso(t);
      const m = new Date(t).getUTCMonth();
      if (m !== lastM) {
        if (wk - lastW >= 3) { // a month that starts too close to the last label goes unnamed
          kids.push(svg('text', { x: 34 + wk * (S + G), y: 12, class: 'tick' }, fmt(s, { month: 'short' })));
          lastW = wk;
        }
        lastM = m;
      }
      const d = dayMap.get(s), after = s > A.last, before = !A.first || s < A.first;
      tips.set(s, weekday(s) + ' · ' + (after ? 'not yet' : before ? 'before witness' : d
        ? count(d.conv, 'conversation') + ' · ' + count(d.turns, 'turn') + ' · ' + count(d.notes, 'note')
        : 'no conversation'));
      const c = svg('rect', {
        class: 'cell', 'data-day': s, x: 34 + wk * (S + G), y: 22 + (k % 7) * (S + G), width: S, height: S,
        rx: Math.min(4, S / 4), fill: before || after ? NODATA : RAMP[shade(d ? d.turns : 0)],
      });
      if (animate) c.style.animationDelay = wk * (wide ? 8 : 25) + 'ms';
      kids.push(c);
    }
    cal.replaceChildren(...kids);
  }
  cal.addEventListener('mousemove', e => {
    const s = e.target.getAttribute('data-day');
    if (!s) { tip.classList.remove('on'); return; }
    tip.textContent = tips.get(s);
    tip.style.left = Math.min(e.clientX + 12, innerWidth - tip.offsetWidth - 8) + 'px';
    tip.style.top = e.clientY - 34 + 'px';
    tip.classList.add('on');
  });
  cal.addEventListener('mouseleave', () => tip.classList.remove('on'));

  const act = el('section', { class: 'act' },
    el('div', { class: 'acthead' },
      el('div', {}, el('div', { class: 'kicker', text: 'Overview' }), actTitle),
      el('div', { class: 'periods' }, ...kindBtns, el('span', { class: 'stepper' }, prev, plabel, next))),
    el('div', { class: 'topsec' }, calbox, stats));

  // ---- Portrait: the left menu, the reading column and the sticky "What backs this" panel.

  const topicOf = new Map();
  for (const nb of M.notebooks) for (const tp of nb.topics) topicOf.set(nb.name + '\n' + tp.key, tp.label);

  const menu = el('nav', { class: 'menu' }, el('b', { text: 'Portrait' }));
  const menuItems = M.portraits.map((p, i) => {
    const b = el('button', { type: 'button', class: 'mi', text: p.kind === 'overall' ? 'Overall' : p.id });
    b.addEventListener('click', () => setPortrait(i));
    return b;
  });
  M.portraits.forEach((p, i) => {
    // A corpus notebook (a feed of files, not conversations) is never framed as "who you are".
    if (p.kind === 'other' && (i === 0 || M.portraits[i - 1].kind !== 'other')) {
      menu.append(el('b', { class: 'group', text: 'Other notebooks' }));
    }
    menu.append(menuItems[i]);
  });

  const kicker = el('div', { class: 'kicker' });
  const ptitle = el('h1');
  const written = el('p', { class: 'written' });
  const chips = el('div', { class: 'chips' });
  const side = el('aside', { class: 'side' });
  let column = el('div', { class: 'portrait' });
  const who = el('section', { class: 'who' });
  if (M.portraits.length) {
    who.append(kicker, ptitle, written, chips, el('div', { class: 'lower' }, column, side));
  } else {
    who.append(el('p', { class: 'nothing' }, 'No portrait yet — run ', el('code', { text: 'witness profile' }), '.'));
    pages.overview.classList.add('bare');
  }

  let P = null, sel = -1, kwOn = -1, spans = [];

  function setPortrait(i) {
    P = M.portraits[i];
    sel = -1;
    kwOn = -1;
    menuItems.forEach((b, k) => b.classList.toggle('on', k === i));
    kicker.textContent = P.kind === 'overall' ? 'Who you are · overall'
      : P.kind === 'notebook' ? 'Who you are · ' + P.id + ' notebook' : 'Notebook · ' + P.id;
    ptitle.textContent = P.kind === 'overall' ? P.title || 'Who you are' : P.id;
    written.replaceChildren('written ' + dateText(P.written),
      ...(P.stale ? [' · run ', el('code', { text: 'witness profile' }), ' to refresh'] : []));
    chips.replaceChildren(...P.keywords.map((k, j) => {
      const c = el('button', { type: 'button', class: 'chip', text: k.word });
      c.addEventListener('click', () => setKeyword(j));
      return c;
    }));
    chips.hidden = !P.keywords.length;

    // One span per sentence, numbered as the stored links number them; bold runs stay bold.
    const col = el('div', { class: 'portrait' });
    spans = [];
    let list = null;
    for (const b of P.blocks) {
      if (b.kind === 'h') { list = null; col.append(el('h3', { text: b.text })); continue; }
      const host = el(b.kind === 'li' ? 'li' : 'p');
      if (b.kind !== 'li') list = null;
      else if (!list) col.append(list = el('ul'));
      (list || col).append(host);
      for (const s of b.sentences) {
        const sp = el('span', { class: 's', 'data-sentence': s.n, tabindex: '0' },
          ...s.runs.map(r => r.b ? el('b', { text: r.t }) : r.t));
        spans[s.n] = sp;
        // Chinese sentences run on without a space between them.
        host.append(sp, /[\u3000-\u30ff\u4e00-\u9fff\uff00-\uffef]$/.test(sp.textContent) ? '' : ' ');
      }
    }
    const pick = e => {
      const sp = e.target.closest('[data-sentence]');
      if (sp) select(+sp.getAttribute('data-sentence'));
    };
    col.addEventListener('click', pick);
    col.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(e); } });
    column.replaceWith(col);
    column = col;

    // Open on the first sentence that cites something, so the panel is never empty.
    const first = P.links.findIndex(l => l.length);
    if (first >= 0 && spans[first]) select(first);
    else showSide(-1);
  }

  function select(n) {
    if (spans[sel]) spans[sel].classList.remove('sel');
    sel = n;
    spans[n].classList.add('sel');
    showSide(n);
  }

  function setKeyword(j) {
    kwOn = kwOn === j ? -1 : j;
    chips.querySelectorAll('.chip').forEach((c, k) => c.classList.toggle('on', k === kwOn));
    const hit = new Set(kwOn < 0 ? [] : P.keywords[kwOn].sentences);
    column.classList.toggle('dim', kwOn >= 0);
    spans.forEach((sp, n) => sp.classList.toggle('kw', hit.has(n)));
  }

  // showSide fills the panel for sentence n (-1: none picked). Its parts rise in one after
  // another, once per click.
  function showSide(n) {
    const say = text => el('p', { class: 'fine', text });
    const parts = [el('div', { class: 'sub', text: 'What backs this' })];
    if (n >= 0) parts.push(el('p', { class: 'said', text: spans[n].textContent }));
    if (!P.sources) parts.push(say("Sources for this portrait aren't recorded yet."));
    else if (n < 0) parts.push(say('Click any sentence to see the traits behind it.'));
    else if (!P.links[n].length) parts.push(say('No traits are tied to this sentence.'));
    else for (const id of P.links[n]) parts.push(traitCard(id));
    parts.forEach((p, i) => {
      p.classList.add('rise');
      p.style.animationDelay = i * 90 + 'ms';
    });
    side.replaceChildren(...parts);
  }

  function traitCard(id) {
    const t = M.traits[id], ev = t.evidence[0];
    const go = el('button', { type: 'button', class: 'go', text: 'see how it changed →' });
    go.addEventListener('click', () => openTrait(id));
    const meta = [t.lens, topicOf.get(t.lens + '\n' + t.dim), count(t.notes, 'note'), Math.round(t.today * 100) + '% sure'];
    return el('div', { class: 'tcard', 'data-trait': id },
      el('div', { class: 'cn', text: t.name }),
      el('div', { class: 'cm', text: meta.join(' · ') + (t.trend === 'up' ? ' ↑' : t.trend === 'down' ? ' ↓' : '') }),
      t.summary ? el('p', { class: 'cs', text: t.summary }) : el('p', { class: 'cs none', text: 'No short summary yet.' }),
      ev && ev.evidence ? el('blockquote', { class: 'quote', text: ev.evidence }) : null,
      ev && ev.evidence ? el('div', { class: 'qd', text: 'from your session on ' + dateText(ev.date) }) : null,
      go);
  }

  // ---- Assemble, then draw. The calendar needs its box laid out to size its squares.

  pages.overview.append(menu, el('main', {}, act, who));
  document.body.append(header, pages.overview, pages.changed, tip);
  drawActivity(true);
  if (M.portraits.length) setPortrait(0);
  showPage(M.e2eStart === 'changed' ? 'changed' : 'overview');
  // Redraw on a WIDTH change only: a redraw driven by height can loop.
  new ResizeObserver(() => {
    if (calbox.clientWidth !== drawnWidth) drawCalendar(byKind[kind][wi], false);
  }).observe(calbox);
})();
