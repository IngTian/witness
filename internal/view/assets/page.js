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
  const pct = v => Math.round(v * 100) + '%';
  const arrow = t => t.trend === 'up' ? ' ↑' : t.trend === 'down' ? ' ↓' : '';
  const calm = matchMedia('(prefers-reduced-motion: reduce)').matches;
  // source says where a note came from. A corpus notebook's notes come from ingested files,
  // not conversations, so "your session" would be untrue for them (ruling P23).
  const corpusLens = new Set(M.notebooks.filter(n => n.corpus).map(n => n.name));
  const source = (t, date) => (corpusLens.has(t.lens) ? 'from a document dated ' : 'from your session on ') + dateText(date);

  // ---- Pages: the header switches between them; state is in memory only.

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
    for (const k in pages) {
      pages[k].hidden = k !== p;
      tabs[k].classList.toggle('on', k === p);
    }
    window.scrollTo(0, 0);
  }
  // openTrait is page 1's door into page 2 ("see how it changed →"): that trait's topic, with
  // the trait open.
  function openTrait(id) {
    const t = M.traits[id];
    for (const nb of books) for (const tp of nb.topics) if (nb.name === t.lens && tp.key === t.dim) setTopic(nb, tp, id);
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
  function tween(node, from, to, show = num) {
    const gen = node.tween = (node.tween || 0) + 1, t0 = performance.now();
    if (calm || from == null || from === to) { node.textContent = show(to); return; }
    const step = now => {
      if (node.tween !== gen) return;
      const k = Math.min(1, (now - t0) / 550);
      node.textContent = show(from + (to - from) * (1 - Math.pow(1 - k, 3)));
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
      // The whole sweep starts within 420 ms (25 ms a week at most), so with the 450 ms fade every
      // window, a 54-week year included, finishes inside the ~900 ms motion budget.
      if (animate) c.style.animationDelay = Math.round(wk * Math.min(25, 420 / cols)) + 'ms';
      kids.push(c);
    }
    cal.replaceChildren(...kids);
  }
  function showTip(e, text) {
    tip.textContent = text;
    tip.style.left = Math.min(e.clientX + 12, innerWidth - tip.offsetWidth - 8) + 'px';
    tip.style.top = e.clientY - 34 + 'px';
    tip.classList.add('on');
  }
  const hideTip = () => tip.classList.remove('on');
  cal.addEventListener('mousemove', e => {
    const s = e.target.getAttribute('data-day');
    if (s) showTip(e, tips.get(s)); else hideTip();
  });
  cal.addEventListener('mouseleave', hideTip);

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
      el('div', { class: 'cm', text: meta.join(' · ') + arrow(t) }),
      t.summary ? el('p', { class: 'cs', text: t.summary }) : el('p', { class: 'cs none', text: 'No short summary yet.' }),
      ev && ev.evidence ? el('blockquote', { class: 'quote', text: ev.evidence }) : null,
      ev && ev.evidence ? el('div', { class: 'qd', text: source(t, ev.date) }) : null,
      go);
  }

  // ---- How you've changed: a menu of topics, the topic's traits over time, and its list of
  // traits beside the sticky panel for the open one. Every chart shares the topic's months
  // axis, from the first of its earliest month to the snapshot.

  const ROWS = 18; // rows before "show all N"
  const SNAP = M.snapshot.slice(0, 10);
  // Conversation notebooks first, then corpus ones under "Other notebooks", as on page 1.
  const books = [...M.notebooks.filter(n => !n.corpus), ...M.notebooks.filter(n => n.corpus)];
  // Hover text is kept here, never in an attribute: a trait's name is archive text, and only
  // textContent may carry archive text into the page.
  const tipOf = new WeakMap();

  const menu2 = el('nav', { class: 'menu' });
  const topicBtns = [];
  books.forEach((nb, i) => {
    if (nb.corpus && (i === 0 || !books[i - 1].corpus)) menu2.append(el('div', { class: 'grp', text: 'Other notebooks' }));
    menu2.append(el('b', i ? { class: 'group', text: nb.name } : { text: nb.name }));
    for (const tp of nb.topics) {
      const b = el('button', { type: 'button', class: 'mi', text: tp.label });
      b.addEventListener('click', () => setTopic(nb, tp));
      topicBtns.push([tp, b]);
      menu2.append(b);
    }
  });

  const tKicker = el('div', { class: 'kicker' });
  const tTitle = el('h1', { class: 'topic' });
  const tLede = el('p', { class: 'lede' });
  const tchart = svg('svg', { class: 'chart', 'data-chart': 'topic' });
  const SORTS = [['evidence', 'most evidence'], ['newest', 'newest'], ['changed', 'most changed']];
  const sortBtns = SORTS.map(([k, label]) => {
    const b = el('button', { type: 'button', text: label });
    b.addEventListener('click', () => { sort = k; render(); });
    return b;
  });
  const search = el('input', { type: 'search', placeholder: 'search', 'aria-label': 'search traits', spellcheck: 'false' });
  search.addEventListener('input', render);
  const rowsBox = el('div', { class: 'rows' });
  const more = el('button', { type: 'button', class: 'more' });
  more.addEventListener('click', () => { showAll = true; render(); });
  const panel = el('aside', { class: 'side trait' });

  let TP = null, ax0 = 0, ax1 = 0, open = null, hotId = null, sort = 'evidence', showAll = false, shownPct = null;
  let topicW = 0, panelW = 0, dotG = null, confSvg = null, weekSvg = null;
  const rowEls = new Map(), dots = new Map();
  const frac = t => Math.min(1, Math.max(0, (t - ax0) / (ax1 - ax0)));
  // nice rounds down to 1, 2 or 5 times a power of ten.
  const nice = v => { const p = Math.pow(10, Math.floor(Math.log10(v))), m = v / p; return (m >= 5 ? 5 : m >= 2 ? 2 : 1) * p; };

  function setTopic(nb, tp, pick) {
    TP = tp;
    topicBtns.forEach(([t, b]) => b.classList.toggle('on', t === tp));
    tKicker.textContent = 'Topic · ' + nb.name;
    tTitle.textContent = tp.label;
    tLede.textContent = count(tp.traits.length, 'trait') + ', each placed on the day of its earliest supporting note.';
    ax0 = ms(tp.axisStart || SNAP.slice(0, 8) + '01');
    ax1 = Math.max(ms(SNAP), ax0 + DAY);
    search.value = '';
    showAll = false;
    hotId = null;
    // Without a pick, the trait with the most notes (the earliest of them on a tie).
    open = pick != null ? pick : tp.traits.reduce((a, b) => M.traits[b].notes > M.traits[a].notes ? b : a);
    drawTopic(true);
    render();
    showTrait();
  }

  // months labels the first of each month on the axis; January says its year when the axis
  // crosses one. A label within 30 px of the last one is skipped.
  function months(kids, x, y) {
    const years = new Date(ax0).getUTCFullYear() !== new Date(ax1).getUTCFullYear();
    let last = -1e9;
    for (const m = new Date(ax0); m.getTime() <= ax1; m.setUTCMonth(m.getUTCMonth() + 1)) {
      const s = iso(m.getTime()), px = x(s);
      if (px - last < 30) continue;
      last = px;
      kids.push(svg('text', { x: px, y, class: 'tick' }, years && !m.getUTCMonth() ? String(m.getUTCFullYear()) : fmt(s, { month: 'short' })));
    }
  }

  // The topic chart: how many of its traits had arrived by each day (a step line with a light
  // fill), and under it one dot per trait on its arrival day, over three rows so dots don't
  // stack. A hidden page has no width; it is drawn when it shows (the ResizeObserver below).
  function drawTopic(animate) {
    const W = topicW = tchart.clientWidth;
    dots.clear();
    if (!W) { tchart.replaceChildren(); return; }
    animate = animate && !calm;
    const L = 30, H = 70, x = s => L + frac(ms(s)) * (W - L);
    const ids = TP.traits.filter(id => M.traits[id].arrival); // already by arrival
    const n = Math.max(1, ids.length), y = v => H - v / n * (H - 8);
    const kids = [];
    const step = Math.max(1, nice(n / 2)); // 50 and 100 for 101 traits
    for (let v = step; v <= n; v += step) {
      kids.push(svg('line', { x1: L, x2: W, y1: y(v), y2: y(v), class: 'grid' }),
        svg('text', { x: L - 6, y: y(v) + 4, class: 'tick', 'text-anchor': 'end' }, num(v)));
    }
    let d = 'M' + L + ',' + H;
    ids.forEach((id, k) => { d += ' H' + x(M.traits[id].arrival).toFixed(1) + ' V' + y(k + 1).toFixed(1); });
    d += ' H' + W;
    kids.push(svg('path', { d: d + ' V' + H + ' Z', class: 'area' + (animate ? ' fadein' : '') }),
      svg('path', { d, class: 'line' + (animate ? ' draw' : ''), pathLength: 1 }));
    dotG = svg('g', {});
    ids.forEach((id, k) => {
      const c = svg('circle', { cx: x(M.traits[id].arrival), cy: H + 12 + (k % 3) * 5, r: 2.6, class: 'dot', 'data-trait': id });
      // Every dot has started by 540 ms, so its 300 ms pop ends inside the motion budget.
      if (animate) { c.classList.add('pop'); c.style.animationDelay = Math.round(120 + k * Math.min(45, 420 / n)) + 'ms'; }
      tipOf.set(c, M.traits[id].name);
      c.addEventListener('mouseenter', () => hot(id));
      c.addEventListener('mouseleave', () => hot(null));
      c.addEventListener('click', () => openRow(id, true));
      dots.set(id, c);
      dotG.append(c);
    });
    kids.push(dotG);
    months(kids, x, H + 42);
    tchart.setAttribute('viewBox', '0 0 ' + W + ' 124');
    tchart.setAttribute('height', 124);
    tchart.replaceChildren(...kids);
    markDots();
  }

  function markDots() {
    dots.forEach((c, id) => {
      c.classList.toggle('sel', id === open);
      c.classList.toggle('hot', id === hotId);
      c.setAttribute('r', id === open || id === hotId ? 5 : 2.6);
    });
    if (dotG) dotG.classList.toggle('dimmed', hotId != null);
  }
  // hot links a dot and its row while either is hovered.
  function hot(id) {
    hotId = id;
    rowEls.forEach((r, k) => r.classList.toggle('hot', k === id));
    markDots();
  }

  // render lays out the list for the sort and the search. The open trait always stays in it:
  // pinned to the top when the first rows or the search would leave it out.
  function render() {
    sortBtns.forEach((b, i) => b.classList.toggle('on', SORTS[i][0] === sort));
    const q = search.value.trim().toLowerCase();
    const list = TP.traits.filter(id => !q || (M.traits[id].name + '\n' + M.traits[id].summary).toLowerCase().includes(q));
    const by = {
      evidence: (a, b) => b.notes - a.notes,
      newest: (a, b) => (b.arrival > a.arrival) - (b.arrival < a.arrival),
      changed: (a, b) => b.changes - a.changes || b.notes - a.notes,
    }[sort];
    list.sort((a, b) => by(M.traits[a], M.traits[b])); // stable: ties keep arrival order
    let shown = showAll || q ? list : list.slice(0, ROWS);
    if (open != null && !shown.includes(open)) shown = [open, ...shown];
    rowEls.clear();
    rowsBox.replaceChildren(...shown.map(row), list.length ? '' : el('p', { class: 'fine', text: 'No trait matches that search.' }));
    more.textContent = 'show all ' + num(list.length);
    more.hidden = showAll || !!q || list.length <= ROWS;
    fitMore();
  }

  function row(id) {
    const t = M.traits[id], on = id === open;
    const head = el('button', { type: 'button', class: 'head', 'aria-expanded': String(on) },
      el('span', { class: 'nm', text: t.name }),
      el('span', { class: 'mt', text: num(t.notes) + ' · ' + pct(t.today) + arrow(t) }));
    const r = el('div', { class: 'row' + (on ? ' open' : '') + (id === hotId ? ' hot' : ''), 'data-trait': id },
      head, el('div', { class: 'detail' }, el('div')));
    head.addEventListener('click', () => openRow(id, false));
    r.addEventListener('mouseenter', () => hot(id));
    r.addEventListener('mouseleave', () => hot(null));
    if (on) fill(r, t);
    rowEls.set(id, r);
    return r;
  }

  // fill writes a row's summary the first time it opens: five lines, then "more".
  function fill(r, t) {
    const box = r.lastChild.firstChild;
    if (box.firstChild) return;
    if (!t.summary) { box.append(el('p', { class: 'none', text: 'No short summary yet.' })); return; }
    const c = el('p', { class: 'summary', text: t.summary });
    const tog = el('button', { type: 'button', class: 'tog', text: 'more' });
    tog.addEventListener('click', () => { tog.textContent = c.classList.toggle('full') ? 'less' : 'more'; });
    box.append(c, tog);
  }
  // fitMore hides the open row's "more" when its summary already fits in five lines. A hidden
  // page can't measure, so the button stays until it can (the ResizeObserver calls this again).
  function fitMore() {
    const c = rowEls.has(open) && rowEls.get(open).querySelector('.summary');
    if (c && c.clientHeight && !c.classList.contains('full')) c.nextSibling.hidden = c.scrollHeight <= c.clientHeight + 1;
  }

  // openRow opens a trait in place, or closes it when it is already open; a dot only opens.
  function openRow(id, fromDot) {
    open = open === id && !fromDot ? null : id;
    if (open != null && !rowEls.has(open)) render(); // a dot can pick a trait the list leaves out
    rowEls.forEach((r, k) => {
      if (k === open) fill(r, M.traits[k]);
      r.classList.toggle('open', k === open);
      r.firstChild.setAttribute('aria-expanded', String(k === open));
    });
    fitMore();
    markDots();
    showTrait();
    if (fromDot) rowEls.get(id).scrollIntoView({ block: 'nearest', behavior: calm ? 'auto' : 'smooth' });
  }

  // showTrait fills the sticky panel for the open trait; its parts rise in one after another.
  function showTrait() {
    if (open == null) {
      confSvg = weekSvg = null;
      panel.replaceChildren(el('p', { class: 'fine', text: 'Pick a trait to see how sure witness is and the notes behind it.' }));
      return;
    }
    const t = M.traits[open], big = el('span', { class: 'big' });
    // Confirmed values only, never decayed ones (trend, trendFrom): decay alone would put a
    // "down" on almost every trait.
    const since = t.trend === 'only' ? "only one value was saved"
      : (t.trend === 'up' ? 'up from ' + pct(t.trendFrom) : t.trend === 'down' ? 'down from ' + pct(t.trendFrom) : 'about level') +
        (t.rewrites ? ' · ' + count(t.rewrites, 'rewrite') : '');
    confSvg = svg('svg', { class: 'chart', 'data-chart': 'confidence' });
    weekSvg = svg('svg', { class: 'chart', 'data-chart': 'weeks' });
    const parts = [
      el('div', { class: 'sub', text: 'How sure witness is' }),
      el('div', { class: 'hero' }, big, el('span', { class: 'since', text: since })),
      t.formed ? el('p', { class: 'formed', text: 'formed ' + (t.formed.date ? dateText(t.formed.date) + ' ' : '') +
        'by merging ' + count(t.formed.merged, 'trait') }) : null,
      el('div', { class: 'charts' }, confSvg, weekSvg),
      t.evidence.length ? el('div', { class: 'sub ev', text: 'Evidence' }) : null,
      ...t.evidence.map(ev => el('div', { class: 'qt' },
        ev.evidence ? el('blockquote', { class: 'quote', text: ev.evidence }) : null,
        el('div', { class: 'cap', text: source(t, ev.date) + (ev.note ? ' · ' + ev.note : '') }))),
    ].filter(Boolean);
    // At most eight parts, 65 ms apart, with a 400 ms rise: done by about 855 ms.
    parts.forEach((p, i) => { p.classList.add('rise'); p.style.animationDelay = i * 65 + 'ms'; });
    panel.replaceChildren(...parts);
    tween(big, shownPct, Math.round(t.today * 100), v => Math.round(v) + '%');
    shownPct = Math.round(t.today * 100);
    drawTrait(true);
  }

  // drawTrait draws the open trait's charts on the topic's months axis: how sure witness was
  // (steps through each review; dashed where only a rewrite's final value is known; dashed down
  // to today's faded value), then, in a band of its own below, the notes per week.
  function drawTrait(animate) {
    if (!confSvg) return;
    const W = panelW = confSvg.clientWidth;
    if (!W) { confSvg.replaceChildren(); weekSvg.replaceChildren(); return; }
    animate = animate && !calm;
    const t = M.traits[open], R = W - 44, x = s => frac(ms(s)) * R, end = x(SNAP);
    const tipped = (node, text) => { tipOf.set(node, text); return node; };
    const fade = animate ? ' fadein' : '';
    // Fitted to the trait: a 0–100% scale would flatten a 65% → 91% climb.
    const lo = Math.max(0, Math.min(0.5, Math.min(t.today, ...t.points.map(p => p.v), ...t.spans.map(s => s.v)) - 0.1));
    const H = 150, y = v => 14 + (1 - (v - lo) / (1 - lo)) * (H - 24);
    const c = [];
    for (const v of [0.25, 0.5, 0.75, 1]) {
      if (v > lo) c.push(svg('line', { x1: 0, x2: R, y1: y(v), y2: y(v), class: 'grid' }), svg('text', { x: 0, y: y(v) - 4, class: 'tick' }, pct(v)));
    }
    const dashed = (a, text) => c.push(svg('line', Object.assign({ class: 'dash' + fade }, a)), tipped(svg('line', Object.assign({ class: 'hit' }, a)), text));
    for (const s of t.spans) dashed({ x1: x(s.from), x2: x(s.to), y1: y(s.v), y2: y(s.v) }, 'final value of this rewrite');
    const P = t.points, last = P[P.length - 1];
    if (last) {
      // A point holds solid until the next point, or until a span starts on or after its day,
      // whichever comes first. A `witness merge` version can sit between two logged points,
      // and drawing the earlier value through it would claim a value witness didn't hold then
      // (spec §5). After a span the line starts again at the next point.
      const at = p => x(p.at).toFixed(1) + ',' + y(p.v).toFixed(1);
      const faded = pct(t.today) !== pct(last.v);
      let d = 'M' + at(P[0]);
      P.forEach((p, k) => {
        const nx = P[k + 1], cut = t.spans.find(s => s.from >= p.at && (!nx || s.from <= nx.at));
        if (cut) d += ' H' + x(cut.from).toFixed(1) + (nx ? ' M' + at(nx) : '');
        else if (nx) d += ' H' + x(nx.at).toFixed(1) + ' V' + y(nx.v).toFixed(1);
        else if (!faded) d += ' H' + end.toFixed(1); // nothing has faded: the last step just holds
      });
      if (faded) dashed({ x1: x(last.at), y1: y(last.v), x2: end, y2: y(t.today) }, 'fading since last confirmed');
      if (d.includes('H')) c.push(svg('path', { d, class: 'line' + (animate ? ' draw' : ''), pathLength: 1 }));
      P.forEach((p, k) => {
        const dot = tipped(svg('circle', { cx: x(p.at), cy: y(p.v), r: 3.5, class: 'pt' }), 'reviewed ' + dateText(p.at) + ' · ' + pct(p.v));
        if (animate) { dot.classList.add('pop'); dot.style.animationDelay = Math.round(200 + k * Math.min(45, 300 / P.length)) + 'ms'; }
        c.push(dot);
      });
    } else c.push(svg('circle', { cx: end, cy: y(t.today), r: 3.5, class: 'pt' }));
    c.push(svg('text', { x: end + 8, y: y(t.today) + 4, class: 'now' }, pct(t.today)));
    confSvg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    confSvg.setAttribute('height', H);
    confSvg.replaceChildren(...c);

    // Notes per week, zero in the weeks between those that have any, each at its midweek.
    const WH = 66, top = 20, base = 46, per = new Map(t.weeks.map(w => [w.w, w.n]));
    const pts = [];
    for (let m = ax0 - ((new Date(ax0).getUTCDay() + 6) % 7) * DAY; m <= ax1; m += 7 * DAY) {
      pts.push([frac(m + 3.5 * DAY) * R, per.get(iso(m)) || 0]);
    }
    const peak = Math.max(...pts.map(p => p[1])), ny = n => base - (peak ? n / peak : 0) * (base - top);
    const line = 'M' + pts.map(([px, n]) => px.toFixed(1) + ',' + ny(n).toFixed(1)).join(' L');
    const w = [svg('text', { x: 0, y: 10, class: 'tick' }, 'notes per week'),
      svg('path', { d: line + ' L' + pts[pts.length - 1][0].toFixed(1) + ',' + base + ' L' + pts[0][0].toFixed(1) + ',' + base + ' Z', class: 'warea' + fade }),
      svg('path', { d: line, class: 'wline' + (animate ? ' draw' : ''), pathLength: 1 })];
    if (peak) { // labelled; moved clear of the band's own label when it sits under it
      const px = pts.find(p => p[1] === peak)[0], left = px < 100;
      w.push(svg('text', { x: left ? Math.max(px + 6, 92) : px, y: ny(peak) - 5, class: 'tick', 'text-anchor': left ? 'start' : px > R - 16 ? 'end' : 'middle' }, num(peak)));
    }
    months(w, x, WH - 2);
    weekSvg.setAttribute('viewBox', '0 0 ' + W + ' ' + WH);
    weekSvg.setAttribute('height', WH);
    weekSvg.replaceChildren(...w);
  }

  pages.changed.addEventListener('mousemove', e => {
    const s = tipOf.get(e.target);
    if (s) showTip(e, s); else hideTip();
  });
  pages.changed.addEventListener('mouseleave', hideTip);

  // ---- Assemble, then draw. The calendar needs its box laid out to size its squares.

  pages.overview.append(menu, el('main', {}, act, who));
  if (books.length) {
    pages.changed.append(menu2, el('main', {}, tKicker, tTitle, tLede, tchart,
      el('div', { class: 'split' },
        el('div', {}, el('div', { class: 'controls' }, ...sortBtns, search), rowsBox, more),
        panel)));
  } else {
    pages.changed.classList.add('bare');
    pages.changed.append(el('main', {}, el('p', { class: 'nothing', text: 'No traits yet.' })));
  }
  document.body.append(header, pages.overview, pages.changed, tip);
  drawActivity(true);
  if (M.portraits.length) setPortrait(0);
  showPage(M.e2eStart === 'changed' ? 'changed' : 'overview');
  // Page 2 opens on the first notebook's first topic until a "see how it changed →" picks one.
  if (books.length) setTopic(books[0], books[0].topics[0]);
  // Redraw on a WIDTH change only: a redraw driven by height can loop. A page that was hidden
  // drew nothing, so its first visible draw is its first draw, and moves.
  new ResizeObserver(() => {
    if (calbox.clientWidth !== drawnWidth) drawCalendar(byKind[kind][wi], false);
  }).observe(calbox);
  new ResizeObserver(() => {
    if (!TP) return;
    if (tchart.clientWidth && tchart.clientWidth !== topicW) drawTopic(!topicW);
    if (confSvg && confSvg.clientWidth && confSvg.clientWidth !== panelW) drawTrait(!panelW);
    fitMore();
  }).observe(pages.changed);
})();
