// Runs the setup page's real script against a fake browser, one scenario per
// outcome of a connection attempt. Invoked by TestSetupPageScript with the
// script extracted from the rendered page; exits non-zero if any scenario fails.
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');

function run(name, routes, expect) {
  return new Promise((resolve) => {
    let now = 0, timers = [], done = false;
    const el = () => ({ style: {}, innerHTML: '', value: '', checked: false, disabled: false,
      className: '', textContent: '', listeners: {}, appendChild() {},
      addEventListener(ev, fn) { this.listeners[ev] = fn; } });
    const els = { ssid: el(), manual: el(), 'ssid-manual': el(), psk: el(), go: el(), msg: el(),
      rescan: el(), 'open-app': el() };
    els.ssid.options = [{}];
    // Remember whether the "open the app" link was ever shown.
    let appLinkShown = false, cls = '', sawDisplayHint = false, msgText = '';
    Object.defineProperty(els.msg, 'textContent', {
      get: () => msgText, set: (v) => { msgText = v; if (/CONNECTED/.test(v)) sawDisplayHint = true; } });
    Object.defineProperty(els['open-app'], 'className', {
      get: () => cls, set: (v) => { cls = v; if (/show/.test(v)) appLinkShown = true; } });
    els.ssid.value = 'Home';
    const location = { href: 'http://10.42.0.1/' };
    const calls = [];
    const fetch = (url, opts) => {
      calls.push(url);
      const step = routes(url, calls.filter((u) => u === url).length, opts);
      if (step === 'down') return Promise.reject(new TypeError('network'));
      return Promise.resolve({ json: () => Promise.resolve(step) });
    };
    const g = {
      document: { getElementById: (id) => els[id], createElement: () => el() },
      window: { AbortController: function () { this.signal = {}; this.abort = () => {}; } },
      location, fetch, JSON, Date: { now: () => now },
      setTimeout: (fn, ms) => { timers.push({ at: now + (ms || 0), fn }); return timers.length; },
      clearTimeout: () => {},
    };
    new Function(...Object.keys(g), src)(...Object.values(g));
    els.go.listeners.click();
    // Drive time forward, letting promises settle between steps.
    (function tick(i) {
      if (i > 3000 || done) return finish();
      timers.sort((a, b) => a.at - b.at);
      const t = timers.shift();
      if (!t) return finish();
      now = Math.max(now, t.at);
      t.fn();
      setImmediate(() => setImmediate(() => tick(i + 1)));
    })(0);
    function finish() {
      if (done) return; done = true;
      const ok = expect({ href: location.href, msg: els.msg.textContent, kind: els.msg.className,
        btn: els.go.disabled, t: now, list: els.ssid.innerHTML, appLinkShown, sawDisplayHint });
      console.log((ok ? 'PASS ' : 'FAIL ') + name + ' | ' + location.href + ' | ' + els.msg.className + ' | ' + els.msg.textContent.slice(0, 90));
      resolve(ok);
    }
  });
}

const APP = 'http://timeblaster.local:8080/api/wifi/status';
const scenarios = [
  ['joined the chosen network', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === '/api/connect' ? 'down' : u === APP ? (n < 3 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) : 'down',
    (r) => r.href === 'http://timeblaster.local:8080/' && /Connected/.test(r.msg)],
  ['wrong password, setup network came back', (u) => u === '/api/networks' ? { networks: [] } :
      u === '/api/status' ? { mode: 'setup', last_error: 'Wrong password.' } : 'down',
    (r) => r.href === 'http://10.42.0.1/' && /Wrong password/.test(r.msg) && !r.btn],
  ['app answers over the setup network in setup mode', (u) => u === '/api/networks' ? { networks: [] } :
      u === APP ? { mode: 'setup', last_error: 'Wrong password.' } : 'down',
    (r) => r.href === 'http://10.42.0.1/' && /Wrong password/.test(r.msg) && !r.btn],
  ['fell back to the previous network', (u) => u === '/api/networks' ? { networks: [] } :
      u === APP ? { mode: 'normal', connected: true, ssid: 'OldNet' } : 'down',
    (r) => r.href === 'http://timeblaster.local:8080/' && /went back to "OldNet"/.test(r.msg)],
  ['refused at once, nothing to watch', (u) => u === '/api/networks' ? { networks: [] } :
      u === '/api/connect' ? { success: false, message: 'Password must be 8-63 characters.' } :
      u === APP ? { mode: 'normal', connected: true, ssid: 'Home' } : 'down',
    (r) => r.href === 'http://10.42.0.1/' && /8-63/.test(r.msg) && !r.btn],
  ['setup started again, no error: says so, not "wrong password"', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === '/api/status' ? (n < 3 ? 'down' : { mode: 'setup' }) : 'down',
    (r) => /started again/.test(r.msg) && !/password/i.test(r.msg) && !r.btn],
  ['a scan that fails keeps the list to choose from', (u) => u === '/api/networks' ? 'down' :
      u === '/api/status' ? { mode: 'setup', last_error: 'Wrong password.' } : 'down',
    (r) => !/Could not scan/.test(r.list) && /Wrong password/.test(r.msg) && !r.btn],
  ['a slow phone gets the app link, then the app', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === APP ? (n < 12 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) : 'down',
    (r) => r.appLinkShown && r.href === 'http://timeblaster.local:8080/' && r.t >= 15000],
  ['setup network still up while joining: not a failure', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === APP ? (n < 3 ? { mode: 'setup', joining: true } : n < 6 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) :
      u === '/api/status' ? 'down' : 'down',
    (r) => r.href === 'http://timeblaster.local:8080/' && /Connected/.test(r.msg)],
  ['a long join, still reachable and "joining": waits it out', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === APP ? (n < 16 ? { mode: 'setup', joining: true } : n < 18 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) : 'down',
    (r) => r.href === 'http://timeblaster.local:8080/' && /Connected/.test(r.msg) && r.t >= 30000],
  ['setup answering early without "joining": waits rather than failing', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === APP ? (n < 3 ? { mode: 'setup' } : n < 6 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) : 'down',
    (r) => r.href === 'http://timeblaster.local:8080/'],
  ['the request never arrived: says so after a while', (u) => u === '/api/networks' ? { networks: [] } :
      u === APP ? { mode: 'setup' } : 'down',
    (r) => /did not start joining/.test(r.msg) && !r.btn && r.t >= 20000],
  ['the gap: points at the display once the setup network has gone', (u, n) => u === '/api/networks' ? { networks: [] } :
      u === APP ? (n < 4 ? 'down' : { mode: 'normal', connected: true, ssid: 'Home' }) : 'down',
    (r) => r.sawDisplayHint && r.href === 'http://timeblaster.local:8080/'],
  ['never appears: gives up after three minutes', (u) => u === '/api/networks' ? { networks: [] } : 'down',
    (r) => r.href === 'http://10.42.0.1/' && /has not appeared/.test(r.msg) && !r.btn && r.t >= 180000],
];
(async () => {
  let all = true;
  for (const [n, routes, expect] of scenarios) all = (await run(n, routes, expect)) && all;
  process.exit(all ? 0 : 1);
})();
