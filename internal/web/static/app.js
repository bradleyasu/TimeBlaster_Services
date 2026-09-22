/* Timeblaster companion app.
 *
 * Two rules shape this file:
 *
 *  1. The Raspberry Pi is the source of truth. The app renders state it is given
 *     and sends commands; it never keeps its own idea of what the device is
 *     doing. Closing the app changes nothing on the device.
 *  2. The WebSocket is the primary channel. HTTP is used for commands and for
 *     the initial load when the socket is unavailable, so the app degrades to
 *     "works but does not live-update" rather than to "broken".
 */
(function () {
  'use strict';

  var state = null;
  var sounds = [];
  var ws = null;
  var wsBackoff = 1000;
  var clockTimer = null;
  // clockOffsetMs is the difference between the Pi's clock and the phone's, so
  // the displayed time ticks locally but stays anchored to the device.
  var clockOffsetMs = 0;
  var editing = null;

  var DAY_NAMES = ['S', 'M', 'T', 'W', 'T', 'F', 'S'];
  var DAY_FULL = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

  function $(id) { return document.getElementById(id); }

  // --- HTTP ---------------------------------------------------------------

  function api(method, path, body) {
    var opts = { method: method, headers: {} };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) return null;
      return r.json().catch(function () { return null; }).then(function (data) {
        if (!r.ok) {
          var msg = (data && (data.error || data.message)) || ('HTTP ' + r.status);
          throw new Error(msg);
        }
        return data;
      });
    });
  }

  var toastTimer = null;
  function toast(msg, isError) {
    var el = $('toast');
    el.textContent = msg;
    el.className = 'toast' + (isError ? ' error' : '');
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.hidden = true; }, 4000);
  }

  function fail(err) { toast(err.message || String(err), true); }

  // --- WebSocket ----------------------------------------------------------

  function connect() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    try {
      ws = new WebSocket(proto + '//' + location.host + '/api/ws');
    } catch (e) {
      scheduleReconnect();
      return;
    }

    ws.onopen = function () {
      wsBackoff = 1000;
      setConnected(true);
    };
    ws.onclose = function () {
      setConnected(false);
      scheduleReconnect();
    };
    ws.onerror = function () { /* onclose follows and handles it */ };
    ws.onmessage = function (ev) {
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      handleEvent(msg);
    };
  }

  function scheduleReconnect() {
    setTimeout(connect, wsBackoff);
    wsBackoff = Math.min(wsBackoff * 2, 20000);
  }

  function setConnected(up) {
    var el = $('conn');
    el.className = 'conn' + (up ? ' up' : '');
    el.title = up ? 'Connected to the Timeblaster' : 'Reconnecting…';
  }

  function handleEvent(msg) {
    switch (msg.type) {
      case 'state':
        applyState(msg.data);
        break;
      case 'alarm_started':
      case 'alarm_stopped':
      case 'alarm_snoozed':
      case 'alarms_changed':
      case 'schedule_changed':
      case 'channel_changed':
      case 'channels_changed':
      case 'volume_changed':
      case 'hardware_changed':
      case 'wifi_changed':
      case 'settings_changed':
        // Every change refetches the full snapshot. The device is one user and
        // a handful of fields; a correct, simple refresh beats incremental
        // patching that can drift out of step.
        refresh();
        break;
    }
  }

  // --- State --------------------------------------------------------------

  function refresh() {
    return api('GET', 'api/state').then(applyState).catch(function () { /* the socket will retry */ });
  }

  function applyState(s) {
    if (!s) return;
    state = s;

    if (s.clock && s.clock.now) {
      clockOffsetMs = new Date(s.clock.now).getTime() - Date.now();
    }
    renderClock();
    renderRinging();
    renderAlarms();
    renderChannels();
    renderSettings();
    renderSystem();
  }

  function deviceNow() { return new Date(Date.now() + clockOffsetMs); }

  function pad(n) { return n < 10 ? '0' + n : String(n); }

  function formatTime(d, force24) {
    var use24 = force24 !== undefined ? force24 : (state && state.clock && state.clock.clock_24h);
    var h = d.getHours(), m = pad(d.getMinutes());
    if (use24) return pad(h) + ':' + m;
    var suffix = h >= 12 ? 'PM' : 'AM';
    var h12 = h % 12 || 12;
    return h12 + ':' + m + ' ' + suffix;
  }

  function formatHM(hour, minute) {
    var d = new Date();
    d.setHours(hour, minute, 0, 0);
    return formatTime(d);
  }

  function renderClock() {
    var now = deviceNow();
    $('clock').textContent = formatTime(now);

    var tz = (state && state.clock && state.clock.timezone) || '';
    $('clock-sub').textContent = now.toLocaleDateString(undefined, {
      weekday: 'long', month: 'short', day: 'numeric'
    }) + (tz ? ' · ' + tz : '');

    var next = state && state.alarms && state.alarms.next;
    $('next-alarm').textContent = next
      ? 'Next alarm ' + describeWhen(new Date(next.at)) + (next.label ? ' · ' + next.label : '')
      : 'No alarms scheduled';
  }

  function describeWhen(at) {
    var now = deviceNow();
    var mins = Math.round((at - now) / 60000);
    if (mins < 60) return 'in ' + Math.max(mins, 0) + ' min';
    if (at.toDateString() === now.toDateString()) return 'today at ' + formatTime(at);
    var tomorrow = new Date(now.getTime() + 86400000);
    if (at.toDateString() === tomorrow.toDateString()) return 'tomorrow at ' + formatTime(at);
    return DAY_FULL[at.getDay()] + ' at ' + formatTime(at);
  }

  function renderRinging() {
    var active = state && state.alarms && state.alarms.active;
    var el = $('ringing');
    if (!active) { el.hidden = true; return; }
    el.hidden = false;
    $('ringing-label').textContent = active.state === 'snoozed'
      ? 'SNOOZED' + (active.snooze_until ? ' UNTIL ' + formatTime(new Date(active.snooze_until)) : '')
      : (active.label ? active.label.toUpperCase() : 'ALARM');
    $('btn-snooze').disabled = active.state !== 'ringing';
  }

  // --- Alarms -------------------------------------------------------------

  function renderAlarms() {
    var list = $('alarm-list');
    list.innerHTML = '';
    var alarms = (state && state.alarms_list) || window.__alarms || [];

    if (!alarms.length) {
      var empty = document.createElement('div');
      empty.className = 'empty';
      empty.textContent = 'No alarms yet. Tap NEW ALARM to add one.';
      list.appendChild(empty);
      return;
    }

    alarms.forEach(function (a) {
      var card = document.createElement('div');
      card.className = 'card' + (a.enabled ? '' : ' off');

      var main = document.createElement('div');
      main.className = 'card-main';
      main.innerHTML =
        '<div class="card-time">' + escapeHTML(formatHM(a.hour, a.minute)) + '</div>' +
        '<div class="card-sub">' + escapeHTML(a.repeat_label) +
        (a.label ? ' · ' + escapeHTML(a.label) : '') +
        (a.sound_id ? ' · ' + escapeHTML(a.sound_id) : '') + '</div>';
      main.addEventListener('click', function () { openEditor(a); });

      var sw = document.createElement('label');
      sw.className = 'switch';
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = !!a.enabled;
      cb.addEventListener('change', function () {
        api('POST', 'api/alarms/' + a.id + '/enabled', { enabled: cb.checked })
          .then(loadAlarms)
          .catch(function (e) { cb.checked = !cb.checked; fail(e); });
      });
      sw.appendChild(cb);
      sw.appendChild(document.createElement('span'));

      card.appendChild(main);
      card.appendChild(sw);
      list.appendChild(card);
    });
  }

  function loadAlarms() {
    return api('GET', 'api/alarms').then(function (d) {
      window.__alarms = (d && d.alarms) || [];
      renderAlarms();
    }).catch(fail);
  }

  function openEditor(a) {
    editing = a || null;
    $('editor-title').textContent = a ? 'EDIT ALARM' : 'NEW ALARM';
    $('ed-hour').value = a ? a.hour : 7;
    $('ed-minute').value = a ? pad(a.minute) : '00';
    $('ed-label').value = a ? (a.label || '') : '';
    $('ed-snooze').value = a ? a.snooze_minutes : 9;
    $('ed-snooze-value').textContent = $('ed-snooze').value;
    $('ed-autostop').value = a ? a.auto_stop_minutes : 15;
    $('ed-autostop-value').textContent = $('ed-autostop').value;
    $('ed-delete').hidden = !a;

    var mask = a ? a.repeat_days : 0;
    var days = $('ed-days');
    days.innerHTML = '';
    DAY_NAMES.forEach(function (name, i) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'day' + ((mask & (1 << i)) ? ' on' : '');
      b.textContent = name;
      b.dataset.bit = String(1 << i);
      b.addEventListener('click', function () { b.classList.toggle('on'); });
      days.appendChild(b);
    });

    fillSoundSelect($('ed-sound'), a ? a.sound_id : '');
    $('editor').hidden = false;
  }

  function closeEditor() { $('editor').hidden = true; editing = null; }

  function saveAlarm() {
    var mask = 0;
    Array.prototype.forEach.call($('ed-days').children, function (b) {
      if (b.classList.contains('on')) mask |= parseInt(b.dataset.bit, 10);
    });

    var body = {
      hour: parseInt($('ed-hour').value, 10) || 0,
      minute: parseInt($('ed-minute').value, 10) || 0,
      label: $('ed-label').value,
      repeat_days: mask,
      sound_id: $('ed-sound').value,
      snooze_minutes: parseInt($('ed-snooze').value, 10),
      auto_stop_minutes: parseInt($('ed-autostop').value, 10)
    };
    if (!editing) body.enabled = true;

    var req = editing
      ? api('PUT', 'api/alarms/' + editing.id, body)
      : api('POST', 'api/alarms', body);

    req.then(function () {
      closeEditor();
      toast('Alarm saved');
      return loadAlarms();
    }).catch(fail);
  }

  function deleteAlarm() {
    if (!editing) return;
    api('DELETE', 'api/alarms/' + editing.id).then(function () {
      closeEditor();
      toast('Alarm deleted');
      return loadAlarms();
    }).catch(fail);
  }

  // --- Television ---------------------------------------------------------

  function renderChannels() {
    var list = $('channel-list');
    list.innerHTML = '';
    var channels = (state && state.channels) || [];
    var current = state && state.media && state.media.current_channel;

    if (!channels.length) {
      var empty = document.createElement('div');
      empty.className = 'empty';
      empty.textContent = (state && state.media && state.media.ersatztv_reachable)
        ? 'ErsatzTV has no channels configured yet.'
        : 'ErsatzTV is not reachable.';
      list.appendChild(empty);
      return;
    }

    channels.forEach(function (c) {
      var card = document.createElement('div');
      card.className = 'card' + (c.number === current ? ' selected' : '');
      card.innerHTML =
        '<div class="card-main">' +
        '<div class="card-time">' + escapeHTML(c.number) + '</div>' +
        '<div class="card-sub">' + escapeHTML(c.name || '') + '</div>' +
        '</div>';
      card.addEventListener('click', function () {
        api('POST', 'api/channels/select', { number: c.number })
          .then(function () { toast('Channel ' + c.number); return refresh(); })
          .catch(fail);
      });
      list.appendChild(card);
    });
  }

  // --- Settings -----------------------------------------------------------

  function fillSoundSelect(sel, selected) {
    sel.innerHTML = '';
    var blank = document.createElement('option');
    blank.value = '';
    blank.textContent = 'Use the default';
    sel.appendChild(blank);
    sounds.forEach(function (s) {
      var o = document.createElement('option');
      o.value = s.id;
      o.textContent = s.name + (s.fallback ? ' (built in)' : '');
      if (s.id === selected) o.selected = true;
      sel.appendChild(o);
    });
  }

  function loadSounds() {
    return api('GET', 'api/sounds').then(function (d) {
      sounds = (d && d.sounds) || [];
      fillSoundSelect($('set-sound'), state && state.settings_default_sound);

      var list = $('sound-list');
      list.innerHTML = '';
      sounds.forEach(function (s) {
        var card = document.createElement('div');
        card.className = 'card';
        card.innerHTML = '<div class="card-main"><div class="card-title">' +
          escapeHTML(s.name) + '</div><div class="card-sub">' +
          escapeHTML(s.id) + (s.fallback ? ' · built in' : '') + '</div></div>';
        var btn = document.createElement('button');
        btn.className = 'btn small';
        btn.textContent = 'PLAY';
        btn.addEventListener('click', function () {
          api('POST', 'api/sounds/' + encodeURIComponent(s.id) + '/preview')
            .then(function () { toast('Playing ' + s.name + ' on the alarm speaker'); })
            .catch(fail);
        });
        card.appendChild(btn);
        list.appendChild(card);
      });
    }).catch(fail);
  }

  function loadSettings() {
    return api('GET', 'api/settings').then(function (s) {
      $('set-timezone').value = s.timezone || '';
      $('set-clock24').checked = !!s.clock_24h;
      $('set-display-on').checked = !!s.display_on;
      $('set-overlay').checked = !!s.channel_overlay_enabled;
      fillSoundSelect($('set-sound'), s.default_sound_id);
    }).catch(fail);
  }

  function saveSettings() {
    api('PUT', 'api/settings', {
      timezone: $('set-timezone').value.trim(),
      clock_24h: $('set-clock24').checked,
      display_on: $('set-display-on').checked,
      default_sound_id: $('set-sound').value,
      channel_overlay_enabled: $('set-overlay').checked
    }).then(function () {
      toast('Settings saved');
      return refresh();
    }).catch(fail);
  }

  function renderSettings() {
    if (!state || !state.audio) return;
    $('volume-readout').textContent = state.audio.volume_percent + '%';
    $('volume-hint').textContent = state.audio.volume_authoritative
      ? 'Set by the physical knob on the Timeblaster.'
      : 'Starting value; the physical knob takes over as soon as it is moved.';
  }

  // --- System -------------------------------------------------------------

  function renderSystem() {
    api('GET', 'api/health').then(function (h) {
      var el = $('health');
      el.innerHTML = '';

      var overall = document.createElement('div');
      overall.className = 'health-row';
      overall.innerHTML = '<span class="name">Overall</span><span class="value ' +
        cssStatus(h.status) + '">' + escapeHTML(h.status.toUpperCase()) + '</span>';
      el.appendChild(overall);

      Object.keys(h.components).sort().forEach(function (k) {
        var row = document.createElement('div');
        row.className = 'health-row';
        row.innerHTML = '<span class="name">' + escapeHTML(k) + '</span>' +
          '<span class="value ' + cssStatus(h.components[k]) + '">' +
          escapeHTML(h.components[k].toUpperCase()) + '</span>';
        el.appendChild(row);
      });

      var up = document.createElement('div');
      up.className = 'health-row';
      up.innerHTML = '<span class="name">Uptime</span><span class="value">' +
        escapeHTML(h.uptime) + '</span>';
      el.appendChild(up);
    }).catch(function () { /* the system tab is best-effort */ });

    var wifi = (state && state.wifi) || {};
    var kv = $('wifi-status');
    kv.innerHTML = '';
    [
      ['Mode', wifi.mode || 'unknown'],
      ['Network', wifi.ssid || '—'],
      ['Address', wifi.ipv4 || '—'],
      ['Hostname', wifi.hostname || (state && state.hostname) || '—']
    ].forEach(function (pair) {
      var row = document.createElement('div');
      row.className = 'kv-row';
      row.innerHTML = '<span class="name">' + escapeHTML(pair[0]) + '</span>' +
        '<span class="value">' + escapeHTML(String(pair[1])) + '</span>';
      kv.appendChild(row);
    });
  }

  function cssStatus(s) {
    if (s === 'ok') return 'ok';
    if (s === 'down' || s === 'unhealthy') return 'down';
    if (s === 'degraded') return 'degraded';
    return 'unknown';
  }

  function escapeHTML(s) {
    return String(s === undefined || s === null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // --- Wiring -------------------------------------------------------------

  function selectTab(name) {
    ['alarms', 'tv', 'settings', 'system'].forEach(function (t) {
      $('tab-' + t).hidden = t !== name;
    });
    Array.prototype.forEach.call(document.querySelectorAll('.tab'), function (b) {
      b.classList.toggle('active', b.dataset.tab === name);
    });
    if (name === 'system') renderSystem();
  }

  function wire() {
    Array.prototype.forEach.call(document.querySelectorAll('.tab'), function (b) {
      b.addEventListener('click', function () { selectTab(b.dataset.tab); });
    });

    $('btn-add').addEventListener('click', function () { openEditor(null); });
    $('ed-save').addEventListener('click', saveAlarm);
    $('ed-cancel').addEventListener('click', closeEditor);
    $('ed-delete').addEventListener('click', deleteAlarm);
    $('editor').addEventListener('click', function (e) {
      if (e.target === $('editor')) closeEditor();
    });
    $('ed-snooze').addEventListener('input', function () {
      $('ed-snooze-value').textContent = this.value;
    });
    $('ed-autostop').addEventListener('input', function () {
      $('ed-autostop-value').textContent = this.value;
    });

    $('btn-dismiss').addEventListener('click', function () {
      api('POST', 'api/alarm/dismiss').then(refresh).catch(fail);
    });
    $('btn-snooze').addEventListener('click', function () {
      api('POST', 'api/alarm/snooze').then(refresh).catch(fail);
    });

    $('btn-refresh-channels').addEventListener('click', function () {
      api('POST', 'api/channels/refresh')
        .then(function () { toast('Refreshing channels…'); setTimeout(refresh, 1500); })
        .catch(fail);
    });
    $('btn-clear-channel').addEventListener('click', function () {
      api('POST', 'api/channels/clear').then(refresh).catch(fail);
    });

    $('btn-save-settings').addEventListener('click', saveSettings);
    $('btn-stop-preview').addEventListener('click', function () {
      api('POST', 'api/sounds/preview/stop').catch(fail);
    });

    $('btn-wifi-setup').addEventListener('click', function () {
      if (!confirm('Start Wi-Fi setup mode? The Timeblaster will leave your network and this page will stop responding.')) return;
      api('POST', 'api/wifi/setup')
        .then(function () { toast('Setup mode starting. Join the TIMEBLASTER-SETUP network.'); })
        .catch(fail);
    });

    // Refetch when the app returns to the foreground: a phone suspends the
    // socket, and the user expects the clock to be right the instant they look.
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) { refresh(); loadAlarms(); }
    });
  }

  function start() {
    wire();
    selectTab('alarms');
    refresh().then(loadSounds).then(loadSettings).then(loadAlarms);
    connect();
    clockTimer = setInterval(renderClock, 1000);

    if ('serviceWorker' in navigator) {
      navigator.serviceWorker.register('sw.js').catch(function () { /* offline support is optional */ });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
