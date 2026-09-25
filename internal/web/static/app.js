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
  // guideData is the last schedule fetched; guideChannel is the channel whose
  // listing is on screen. Both are plain data -- nothing about the channel
  // lineup is baked into this file.
  var guideData = null;
  var guideChannel = null;
  // The daemon version this page first saw. The assets are compiled into the
  // binary, so a different version later means the code has been redeployed and
  // this page is stale. It is only a baseline from the first health response
  // onwards -- a deploy before the System tab is ever opened cannot be spotted,
  // which is why the button does not depend on it.
  var loadedVersion = null;

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

  // The alarm model stores an hour of 0-23 whatever the user's preference. The
  // editor shows whichever form matches the setting and converts at the edges,
  // so the two never disagree about what "7" means.
  function editorUses24h() {
    return !!(state && state.clock && state.clock.clock_24h);
  }

  function setEditorTime(hour24, minute) {
    var hourEl = $('ed-hour');
    var mer = $('ed-meridiem');
    $('ed-minute').value = pad(minute);

    if (editorUses24h()) {
      mer.hidden = true;
      hourEl.min = 0;
      hourEl.max = 23;
      hourEl.value = pad(hour24);
      return;
    }

    mer.hidden = false;
    hourEl.min = 1;
    hourEl.max = 12;
    hourEl.value = hour24 % 12 || 12;   // midnight and noon both show as 12
    var pm = hour24 >= 12;
    Array.prototype.forEach.call(mer.children, function (b) {
      b.classList.toggle('on', (b.dataset.m === 'pm') === pm);
    });
  }

  // editorTime reads the entered time, or reports why it cannot.
  //
  // It rejects rather than clamps. A number input does not stop anyone typing
  // 75, and quietly turning that into 23 -- or into midnight, in 12-hour mode
  // -- would set an alarm for a time the user never asked for. An alarm going
  // off at the wrong hour is the one failure an alarm clock cannot have, and a
  // silent correction is indistinguishable from it having worked.
  function editorTime() {
    var use24 = editorUses24h();
    var hourMin = use24 ? 0 : 1;
    var hourMax = use24 ? 23 : 12;

    var hourRaw = String($('ed-hour').value).trim();
    var minRaw = String($('ed-minute').value).trim();
    var hour = parseInt(hourRaw, 10);
    var minute = parseInt(minRaw, 10);

    if (hourRaw === '' || isNaN(hour) || hour < hourMin || hour > hourMax) {
      return { field: 'ed-hour', error: 'Hour must be between ' + hourMin + ' and ' + hourMax + '.' };
    }
    if (minRaw === '' || isNaN(minute) || minute < 0 || minute > 59) {
      return { field: 'ed-minute', error: 'Minutes must be between 0 and 59.' };
    }

    if (!use24) {
      // 12 AM is hour 0 and 12 PM is hour 12, so the modulo has to come before
      // the twelve-hour shift rather than after it.
      hour = hour % 12;
      var chosen = $('ed-meridiem').querySelector('.day.on');
      if (chosen && chosen.dataset.m === 'pm') hour += 12;
    }
    return { hour: hour, minute: minute };
  }

  function openEditor(a) {
    editing = a || null;
    $('editor-title').textContent = a ? 'EDIT ALARM' : 'NEW ALARM';
    setEditorTime(a ? a.hour : 7, a ? a.minute : 0);
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
    var time = editorTime();
    if (time.error) {
      var bad = $(time.field);
      bad.classList.add('invalid');
      bad.focus();
      if (bad.select) bad.select();
      toast(time.error);
      return;
    }

    var mask = 0;
    Array.prototype.forEach.call($('ed-days').children, function (b) {
      if (b.classList.contains('on')) mask |= parseInt(b.dataset.bit, 10);
    });

    var body = {
      hour: time.hour,
      minute: time.minute,
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

  // --- TV guide -------------------------------------------------------------

  // The guide is built entirely from what the server sends. Channels, their
  // numbers and their names all arrive as data, so adding a channel in ErsatzTV
  // makes it appear here with no change to this file.

  function loadGuide(force) {
    var list = $('guide-list');
    if (force || !guideData) {
      list.innerHTML = '<div class="empty">Loading the schedule\u2026</div>';
    }
    return api('GET', 'api/guide?hours=48')
      .then(function (g) { guideData = g; renderGuide(); })
      .catch(function (e) {
        list.innerHTML = '';
        var empty = document.createElement('div');
        empty.className = 'empty';
        empty.textContent = 'Could not load the guide: ' + e.message;
        list.appendChild(empty);
      });
  }

  function renderGuide() {
    var chips = $('guide-channels');
    var list = $('guide-list');
    chips.innerHTML = '';
    list.innerHTML = '';

    var channels = (guideData && guideData.channels) || [];
    if (!channels.length) {
      $('guide-hint').textContent = '';
      list.appendChild(emptyRow('ErsatzTV has no channels configured yet.'));
      return;
    }

    // Default to whatever is on the television, so opening the guide answers
    // "what am I watching" without a tap.
    var current = state && state.media && state.media.current_channel;
    var known = function (n) { return channels.some(function (c) { return c.number === n; }); };
    if (!guideChannel || !known(guideChannel)) {
      guideChannel = (current && known(current)) ? current : channels[0].number;
    }

    channels.forEach(function (c) {
      var chip = document.createElement('button');
      chip.className = 'chip' + (c.number === guideChannel ? ' active' : '');
      chip.textContent = c.number + (c.name ? ' ' + c.name : '');
      chip.addEventListener('click', function () { guideChannel = c.number; renderGuide(); });
      chips.appendChild(chip);
    });

    var chan = channels.filter(function (c) { return c.number === guideChannel; })[0];
    var progs = (chan && chan.programmes) || [];
    if (!progs.length) {
      $('guide-hint').textContent = '';
      // A channel with no playout is still listed, with nothing scheduled.
      // Saying so beats leaving it out and looking broken.
      list.appendChild(emptyRow('Nothing is scheduled on this channel.'));
      return;
    }
    $('guide-hint').textContent = progs.length + ' programmes \u00b7 tap one to tune this channel';

    var nowMs = Date.now() + clockOffsetMs;
    var lastDay = '';
    var html = '';
    progs.forEach(function (p) {
      var start = new Date(p.start);
      var stop = new Date(p.stop);
      var day = dayLabel(start, nowMs);
      if (day !== lastDay) {
        html += '<div class="guide-day">' + escapeHTML(day) + '</div>';
        lastDay = day;
      }
      var on = nowMs >= start.getTime() && nowMs < stop.getTime();
      html += '<div class="guide-row' + (on ? ' now' : '') + '">' +
        '<div class="guide-time">' + escapeHTML(formatTime(start)) + '</div>' +
        '<div class="guide-body">' +
        '<div class="guide-title">' + escapeHTML(p.title || 'Untitled') +
        (on ? '<span class="guide-badge">NOW</span>' : '') + '</div>' +
        (p.subTitle ? '<div class="guide-sub">' + escapeHTML(p.subTitle) + '</div>' : '') +
        '</div></div>';
    });
    list.innerHTML = html;

    var onNow = list.querySelector('.guide-row.now');
    if (onNow && onNow.scrollIntoView) onNow.scrollIntoView({ block: 'center' });
  }

  function emptyRow(text) {
    var d = document.createElement('div');
    d.className = 'empty';
    d.textContent = text;
    return d;
  }

  function dayLabel(d, nowMs) {
    var same = function (a, b) {
      return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() &&
        a.getDate() === b.getDate();
    };
    if (same(d, new Date(nowMs))) return 'TODAY';
    if (same(d, new Date(nowMs + 86400000))) return 'TOMORROW';
    return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' }).toUpperCase();
  }

  // --- Settings -------------------------------------------------------------

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
      // Keep whatever is already chosen. loadSettings owns which sound is the
      // default and fills this from /api/settings; guessing from a state key
      // that does not exist only risked disagreeing with it.
      fillSoundSelect($('set-sound'), $('set-sound').value);

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

  function renderAppVersion(serverVersion) {
    var kv = $('app-version');
    kv.innerHTML = '';
    var stale = loadedVersion !== null && serverVersion !== loadedVersion;

    var rows = [['Running', serverVersion || '\u2014']];
    if (stale) rows.push(['This page', loadedVersion]);

    rows.forEach(function (pair) {
      var row = document.createElement('div');
      row.className = 'kv-row';
      row.innerHTML = '<span class="name">' + escapeHTML(pair[0]) + '</span>' +
        '<span class="value' + (stale ? ' degraded' : '') + '">' +
        escapeHTML(String(pair[1])) + '</span>';
      kv.appendChild(row);
    });

    // A label rather than an automatic reload: the user might be part-way
    // through editing an alarm, and pulling the page out from under them to
    // save a tap would be rude.
    $('btn-refresh-app').textContent = stale ? 'REFRESH APP \u2014 UPDATE READY' : 'REFRESH APP';
  }

  // refreshApp reloads the companion app.
  //
  // Everything is served with Cache-Control: no-cache, so the browser
  // revalidates and an ordinary reload already picks up new code. The service
  // worker and Cache Storage are cleared first anyway: sw.js does not register
  // over plain HTTP today, but it would the moment this is served over HTTPS,
  // and a stale worker would then quietly serve the old app forever.
  //
  // location.reload(true) is deliberately not used -- the argument has been
  // ignored by every current browser for years.
  function refreshApp() {
    var cleanup = Promise.resolve();

    if (navigator.serviceWorker && navigator.serviceWorker.getRegistrations) {
      cleanup = cleanup.then(function () {
        return navigator.serviceWorker.getRegistrations().then(function (regs) {
          return Promise.all(regs.map(function (r) { return r.unregister(); }));
        });
      }).catch(function () { /* nothing registered, or not permitted */ });
    }

    if (window.caches && caches.keys) {
      cleanup = cleanup.then(function () {
        return caches.keys().then(function (keys) {
          return Promise.all(keys.map(function (k) { return caches.delete(k); }));
        });
      }).catch(function () { /* no Cache Storage to clear */ });
    }

    // Reload either way: failing to clear a cache that may not even exist must
    // not leave the button doing nothing.
    cleanup.then(function () { location.reload(); }, function () { location.reload(); });
  }

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

      if (loadedVersion === null) loadedVersion = h.version;
      renderAppVersion(h.version);
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
    ['alarms', 'tv', 'guide', 'settings', 'system'].forEach(function (t) {
      $('tab-' + t).hidden = t !== name;
    });
    Array.prototype.forEach.call(document.querySelectorAll('.tab'), function (b) {
      b.classList.toggle('active', b.dataset.tab === name);
    });
    if (name === 'system') renderSystem();
    // Fetched on first open rather than at start-up: it is a couple of days of
    // scheduling and most sessions never look at it.
    if (name === 'guide' && !guideData) loadGuide();
  }

  function wire() {
    Array.prototype.forEach.call(document.querySelectorAll('.tab'), function (b) {
      b.addEventListener('click', function () { selectTab(b.dataset.tab); });
    });

    $('btn-refresh-app').addEventListener('click', function () {
      toast('Reloading\u2026');
      refreshApp();
    });

    $('btn-refresh-guide').addEventListener('click', function () { loadGuide(true); });

    // Delegated, and bound once here rather than inside renderGuide: that
    // function re-runs on every channel chip, so binding there would stack a
    // fresh listener each time and fire one request per past render.
    //
    // The listing shows a single channel at a time, so any row in it
    // unambiguously means "tune this channel" -- you cannot tune to a
    // programme that has not started, only to the channel carrying it.
    $('guide-list').addEventListener('click', function (e) {
      var row = e.target.closest ? e.target.closest('.guide-row') : null;
      if (!row || !guideChannel) return;
      var number = guideChannel;
      api('POST', 'api/channels/select', { number: number })
        .then(function () { toast('Channel ' + number); return refresh(); })
        .catch(fail);
    });

    $('btn-add').addEventListener('click', function () { openEditor(null); });
    // Mutually exclusive, like a radio pair: tapping one always leaves exactly
    // one selected, so editorHour24 never has to guess.
    Array.prototype.forEach.call($('ed-meridiem').children, function (b) {
      b.addEventListener('click', function () {
        Array.prototype.forEach.call($('ed-meridiem').children, function (o) {
          o.classList.toggle('on', o === b);
        });
      });
    });

    ['ed-hour', 'ed-minute'].forEach(function (id) {
      $(id).addEventListener('input', function () { $(id).classList.remove('invalid'); });
    });

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

  // iOS fires these non-standard gesture events for pinch, and older versions
  // honour them even where touch-action would otherwise have stopped it. They
  // are the last of the three layers described in app.css.
  function refuseZoomGestures() {
    ['gesturestart', 'gesturechange', 'gestureend'].forEach(function (name) {
      document.addEventListener(name, function (e) { e.preventDefault(); }, { passive: false });
    });
    // Double-tap zoom needs nothing here: a touch-action value that does not
    // include zoom disables double-tap as well as pinch. Swallowing touchend
    // to block it would also swallow the click that follows, so tapping two
    // guide rows in quick succession would lose the second one.
  }

  function start() {
    wire();
    refuseZoomGestures();
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
