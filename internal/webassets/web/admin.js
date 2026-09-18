(() => {
  const $ = id => document.getElementById(id);
  const els = {
    serverBadge: $('serverBadge'), logout: $('logoutButton'), grid: $('cameraGrid'), empty: $('cameraEmpty'),
    counter: $('cameraCounter'), title: $('cameraTitle'), video: $('mainVideo'), placeholder: $('placeholder'),
    badge: $('videoBadge'), stats: $('videoStats'), resolution: $('statResolution'), fps: $('statFps'),
    bitrate: $('statBitrate'), rtt: $('statRtt'), jitter: $('statJitter'), loss: $('statLoss'), route: $('statRoute'),
    toast: $('toast'), reconnect: $('reconnectVideoButton'), stop: $('stopVideoButton'), viewers: $('cameraViewers'),
    currentUser: $('currentUser'),
    servoLeft: $('servoLeftButton'), servoRight: $('servoRightButton'),
    servoAngle: $('servoAngle'), servoAngleInput: $('servoAngleInput'), servoConnection: $('servoConnection'),
  };

  const storedViewerId = sessionStorage.getItem('robrouteViewerId');
  const state = {
    username: '', viewerId: validViewerId(storedViewerId) ? storedViewerId : newViewerId(),
    ws: null, wsGeneration: 0, reconnectWsTimer: null, booting: false,
    cameras: [], selected: null, sessionId: null, role: null,
    pc: null, remoteStream: null, pendingRemoteIce: [],
    relayOutbound: new Map(), pendingRelayStarts: new Map(),
    iceServers: [], maxViewersPerCamera: 4,
    negotiationTimer: null, retryTimer: null, retryCount: 0, manualStopped: false,
    statsTimer: null, lastBytes: null, lastAt: null, lastFramesDecoded: null,
    lastFrameProgressAt: null, mediaState: 'idle', stallRestarting: false,
    pendingCriticalSignals: new Map(), pendingLocalIce: [],
  };
  sessionStorage.setItem('robrouteViewerId', state.viewerId);

  async function boot() {
    if (state.booting) return;
    state.booting = true;
    try {
      const me = await fetch('/api/me');
      if (!me.ok) return location.assign('/login');
      const meJson = await me.json();
      state.username = meJson.username || '';
      if (els.currentUser) els.currentUser.textContent = state.username;

      const cfg = await fetch('/api/webrtc-config');
      if (cfg.ok) {
        const json = await cfg.json();
        state.iceServers = json.iceServers || [];
        state.maxViewersPerCamera = json.maxViewersPerCamera || 4;
      }

      const tokenRes = await fetch('/api/ws-token', {
        method: 'POST', headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ viewerId: state.viewerId, activeSessionId: state.sessionId }),
      });
      if (!tokenRes.ok) throw new Error('WS token');
      const tokenJson = await tokenRes.json();
      if (tokenJson.viewerId) {
        state.viewerId = tokenJson.viewerId;
        sessionStorage.setItem('robrouteViewerId', state.viewerId);
      }
      connectWs(tokenJson.token);
    } catch (e) {
      showToast(`Ошибка подключения signaling: ${e.message}`);
      scheduleWsReconnect();
    } finally { state.booting = false; }
  }

  function scheduleWsReconnect(delay = 1200) {
    clearTimeout(state.reconnectWsTimer);
    state.reconnectWsTimer = setTimeout(() => { state.reconnectWsTimer = null; boot(); }, delay);
  }

  function connectWs(token) {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/ws/admin?token=${encodeURIComponent(token)}`);
    const generation = ++state.wsGeneration;
    state.ws = ws;

    ws.onopen = () => {
      if (state.ws !== ws || generation !== state.wsGeneration) return;
      setServer(true);
    };
    ws.onclose = () => {
      if (state.ws !== ws || generation !== state.wsGeneration) return;
      state.ws = null;
      setServer(false);
      // Media stays alive during signaling grace; do not close any PeerConnections here.
      if (state.pc?.connectionState === 'connected' && state.mediaState === 'live') setBadge(`${roleLabel()} live · signaling offline`);
      else if (state.sessionId) setBadge(`${roleLabel()} · signaling reconnect`);
      scheduleWsReconnect();
      updateControls();
    };
    ws.onerror = () => { if (state.ws === ws) setServer(false); };
    ws.onmessage = async event => {
      if (state.ws !== ws || generation !== state.wsGeneration) return;
      let msg; try { msg = JSON.parse(event.data); } catch { return; }

      if (msg.type === 'admin_ready') {
        state.cameras = msg.cameras || [];
        state.iceServers = msg.iceServers || state.iceServers;
        state.maxViewersPerCamera = msg.maxViewersPerCamera || state.maxViewersPerCamera;
        const resumed = msg.resumedSessionId || null;
        if (state.sessionId && resumed !== state.sessionId) {
          // Server forgot the session (grace expired / restart). Drop local media without sending stale STOP.
          closeLocalMedia({ keepSelection: true, keepSession: false, badge: 'idle' });
        } else if (resumed && state.sessionId === resumed) {
          if (state.pc) send({ type: 'webrtc_state', sessionId: resumed, state: state.pc.connectionState || 'connecting' });
          if (state.mediaState === 'live') send({ type: 'webrtc_media', sessionId: resumed, state: 'live' });
        }
        flushBrowserSignals();
        render();
        maybeResumeSelected(250);
        return;
      }
      if (msg.type === 'servo_state') {
        applyServoState(msg.cameraId, msg.servo);
        return;
      }
      if (msg.type === 'servo_error') {
        if (msg.cameraId === state.selected) showToast(`Серво ${msg.code || 'ERROR'}${msg.detail ? `: ${msg.detail}` : ''}`, 6000);
        return;
      }
      if (msg.type === 'camera_list') {
        const wasReady = selectedCameraReady();
        state.cameras = msg.cameras || [];
        render();
        if (!wasReady && selectedCameraReady()) maybeResumeSelected(250);
        return;
      }
      if (msg.type === 'watch_role') {
        if (!isCurrentSessionMessage(msg)) return;
        const nextRole = msg.role === 'relay' ? 'relay' : 'source';
        if (state.role && state.role !== nextRole) closeMainPcKeepSession();
        clearNegotiationTimer();
        state.role = nextRole;
        if (nextRole === 'source') armNegotiationTimeout(state.sessionId);
        setBadge(nextRole === 'source' ? 'SOURCE · connecting phone' : `RELAY · waiting ${msg.sourceUsername || 'source'}`);
        updateControls();
        return;
      }
      if (msg.type === 'webrtc_offer') { await acceptMainOffer(msg, 'source'); return; }
      if (msg.type === 'webrtc_ice') { await acceptMainRemoteIce(msg); return; }
      if (msg.type === 'relay_wait') {
        if (!isCurrentSessionMessage(msg)) return;
        state.role = 'relay';
        setBadge(`RELAY · waiting ${msg.sourceUsername || 'source'}`);
        return;
      }
      if (msg.type === 'relay_offer') { await acceptMainOffer(msg, 'relay'); return; }
      if (msg.type === 'relay_ice') {
        if (isCurrentSessionMessage(msg)) await acceptMainRemoteIce(msg);
        else await acceptOutboundRelayIce(msg);
        return;
      }
      if (msg.type === 'relay_start') { await startOutboundRelay(msg); return; }
      if (msg.type === 'relay_answer') { await acceptOutboundRelayAnswer(msg); return; }
      if (msg.type === 'relay_stop') { closeOutboundRelay(String(msg.sessionId || '')); return; }
      if (msg.type === 'relay_reset') {
        if (!isCurrentSessionMessage(msg)) return;
        clearNegotiationTimer();
        cancelRtcRetry();
        closeMainPcKeepSession();
        state.role = 'relay';
        state.mediaState = 'waiting';
        setBadge('RELAY · source switching');
        return;
      }
      if (msg.type === 'webrtc_stop') {
        if (!isCurrentSessionMessage(msg)) return;
        const reason = msg.reason || 'stopped';
        closeLocalMedia({ keepSelection: true, keepSession: false, badge: 'idle' });
        if (!state.manualStopped && reason !== 'viewer-stopped') scheduleRtcRetry();
        return;
      }
      if (msg.type === 'webrtc_error') {
        if (!isCurrentSessionMessage(msg)) return;
        const code = msg.code || 'WEBRTC_ERROR';
        const detail = msg.detail ? `: ${msg.detail}` : '';
        showToast(`WebRTC ${code}${detail}`, 7000);
        closeLocalMedia({ keepSelection: true, keepSession: false, badge: 'failed' });
        if (!state.manualStopped && code !== 'CAMERA_VIEWER_LIMIT') scheduleRtcRetry();
        return;
      }
    };
  }

  function send(payload) {
    if (state.ws?.readyState === WebSocket.OPEN) {
      try { state.ws.send(JSON.stringify(payload)); return true; } catch { }
    }
    queueBrowserSignal(payload);
    return false;
  }
  function queueBrowserSignal(payload) {
    const sid = String(payload.sessionId || '');
    if (payload.type === 'webrtc_ice' || payload.type === 'relay_ice') {
      if (state.pendingLocalIce.length >= 192) state.pendingLocalIce.shift();
      state.pendingLocalIce.push(payload);
      return;
    }
    const key = `${payload.type}:${sid}`;
    state.pendingCriticalSignals.set(key, payload);
    while (state.pendingCriticalSignals.size > 48) {
      const first = state.pendingCriticalSignals.keys().next().value;
      state.pendingCriticalSignals.delete(first);
    }
  }
  function dropQueuedSignalsForSession(sid) {
    for (const [key, payload] of state.pendingCriticalSignals) if (String(payload.sessionId || '') === sid) state.pendingCriticalSignals.delete(key);
    state.pendingLocalIce = state.pendingLocalIce.filter(p => String(p.sessionId || '') !== sid);
  }
  function flushBrowserSignals() {
    if (state.ws?.readyState !== WebSocket.OPEN) return;
    for (const [key, payload] of [...state.pendingCriticalSignals]) {
      if (!send(payload)) return;
      state.pendingCriticalSignals.delete(key);
    }
    while (state.pendingLocalIce.length) {
      const payload = state.pendingLocalIce[0];
      if (!send(payload)) return;
      state.pendingLocalIce.shift();
    }
  }

  function setServer(ok) {
    els.serverBadge.textContent = `Server: ${ok ? 'online' : 'offline'}`;
    els.serverBadge.className = `status-badge ${ok ? 'online' : 'offline'}`;
  }
  function selectedCameraReady() {
    const c = state.cameras.find(x => x.id === state.selected);
    return Boolean(c?.online && c?.available);
  }
  function render() {
    const online = state.cameras.filter(c => c.online && c.available);
    els.counter.textContent = `${online.length} online`;
    const hasKnownCameras = state.cameras.length > 0;
    els.empty.hidden = hasKnownCameras;
    // Belt-and-suspenders: old styles.css declared .empty-state { display:grid },
    // which can make the empty message visible despite the hidden attribute.
    els.empty.style.display = hasKnownCameras ? 'none' : '';
    els.grid.replaceChildren(...state.cameras.map(camera => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = `robot-card ${state.selected === camera.id ? 'selected' : ''}`;
      btn.disabled = !camera.online || !camera.available;
      const viewers = (camera.viewers || []).map(v => `${v.username}${v.role === 'source' ? ' ★' : ''}`);
      const viewerText = `${camera.viewerCount || 0}/${camera.maxViewers || state.maxViewersPerCamera} viewers${viewers.length ? ` · ${viewers.join(', ')}` : ''}`;
      const signaling = camera.signaling === 'grace' ? ' · SIGNALING RECONNECT' : '';
      btn.innerHTML = `<strong>${escapeHtml(camera.name || camera.id)}</strong><span>${camera.online && camera.available ? `WEBRTC READY${signaling}` : 'OFFLINE'}</span><small>${camera.width || '—'}×${camera.height || '—'} @ ${camera.fps || '—'} FPS</small><small>${escapeHtml(viewerText)}</small>`;
      btn.onclick = () => selectCamera(camera.id);
      return btn;
    }));
    const selected = state.cameras.find(c => c.id === state.selected);
    els.title.textContent = selected ? (selected.name || selected.id) : 'Выберите телефон';
    renderViewers(selected);
    updateControls();
    if (state.selected && (!selected || !selected.online || !selected.available)) {
      closeLocalMedia({ keepSelection: true, keepSession: false, badge: selected?.online ? 'camera unavailable' : 'camera offline' });
    }
  }
  function renderViewers(camera) {
    if (!els.viewers) return;
    if (!camera) return void (els.viewers.textContent = 'Viewers: —');
    const list = (camera.viewers || []).map(v => {
      const marker = v.mediaState === 'live' ? '●' : (v.mediaState === 'stalled' ? '!' : '○');
      const role = v.role === 'source' ? 'source' : 'relay';
      return `${marker} ${v.username} (${role})`;
    });
    els.viewers.textContent = `Viewers: ${camera.viewerCount || 0}/${camera.maxViewers || state.maxViewersPerCamera}${list.length ? ` · ${list.join(', ')}` : ''}`;
  }
  function selectedCamera() { return state.cameras.find(c => c.id === state.selected) || null; }
  function applyServoState(cameraId, servo) {
    if (!cameraId || !servo) return;
    const camera = state.cameras.find(c => c.id === cameraId);
    if (camera) camera.servo = servo;
    if (cameraId === state.selected) renderServo(camera);
  }
  function renderServo(camera) {
    if (!els.servoAngle) return;
    const servo = camera?.servo || null;
    const online = Boolean(camera?.online);
    if (!camera || !servo?.available) els.servoAngle.textContent = 'Поточний кут: —';
    else if (servo.pending) els.servoAngle.textContent = `Поточний кут: ${servo.angle}° → ${servo.targetAngle}°...`;
    else els.servoAngle.textContent = `Поточний кут: ${servo.angle}°`;
    els.servoAngle.classList.toggle('servo-offline', !servo?.available);
    if (els.servoConnection) {
      els.servoConnection.textContent = `ESP32: ${servo?.available ? 'connected' : 'disconnected'}`;
      els.servoConnection.className = `status-badge ${servo?.available ? 'online' : 'offline'}`;
    }
    if (els.servoAngleInput) {
      if (Number.isFinite(Number(servo?.min))) els.servoAngleInput.min = String(servo.min);
      if (Number.isFinite(Number(servo?.max))) els.servoAngleInput.max = String(servo.max);
    }
    const disabled = !online || !servo?.available || state.ws?.readyState !== WebSocket.OPEN;
    for (const btn of [els.servoLeft, els.servoRight]) if (btn) btn.disabled = disabled;
    if (els.servoAngleInput) els.servoAngleInput.disabled = disabled;
  }
  function sendServo(action, angle = null) {
    const camera = selectedCamera();
    if (!camera || !camera.online || !camera.servo?.available) return;
    const payload = { type: 'servo_command', cameraId: camera.id, action };
    if (action === 'set') payload.angle = angle;
    if (!send(payload)) {
      showToast('Нет связи с сервером — команда поворота не отправлена');
    }
  }

  function sendAbsoluteServoAngle() {
    const camera = selectedCamera();
    const raw = els.servoAngleInput?.value?.trim() || '';
    const value = Number(raw);
    const min = Number(camera?.servo?.min);
    const max = Number(camera?.servo?.max);
    if (!raw || !Number.isInteger(value) || !Number.isFinite(min) || !Number.isFinite(max) || value < min || value > max) {
      showToast(`Введіть цілий кут від ${Number.isFinite(min) ? min : 0}° до ${Number.isFinite(max) ? max : 180}°`);
      return;
    }
    sendServo('set', value);
  }

  function updateControls() {
    const hasSelection = Boolean(state.selected);
    if (els.reconnect) els.reconnect.disabled = !hasSelection || !selectedCameraReady() || state.ws?.readyState !== WebSocket.OPEN;
    if (els.stop) els.stop.disabled = !hasSelection || !state.sessionId;
    renderServo(selectedCamera());
  }

  function selectCamera(id) {
    const same = state.selected === id;
    state.selected = id;
    state.manualStopped = false;
    state.retryCount = 0;
    render();
    if (same) return restartRtc(true);
    stopCurrentSession(true, true);
    startRtc(id);
  }
  function startRtc(cameraId) {
    if (!cameraId || state.manualStopped) return;
    if (state.ws?.readyState !== WebSocket.OPEN || !selectedCameraReady()) return scheduleRtcRetry(1000);
    cancelRtcRetry(); clearNegotiationTimer(); resetMediaWatchdog();
    state.pendingRemoteIce = [];
    state.sessionId = newSessionId();
    state.role = null;
    state.mediaState = 'waiting';
    setBadge('requesting'); updateControls();
    const sid = state.sessionId;
    if (!send({ type: 'webrtc_start', cameraId, sessionId: sid })) {
      // A send/close race can queue this START after we already abandoned the session.
      // Never let a stale session consume a viewer slot after signaling reconnects.
      dropQueuedSignalsForSession(sid);
      state.sessionId = null; scheduleRtcRetry(1000); return;
    }
  }

  async function acceptMainOffer(msg, mode) {
    const sid = String(msg.sessionId || '');
    if (sid !== state.sessionId || msg.cameraId !== state.selected || !msg.sdp) return;
    if (state.pc) return;
    if (mode === 'source' && state.role && state.role !== 'source') return;
    if (mode === 'relay' && state.role && state.role !== 'relay') return;
    state.role = mode;
    armNegotiationTimeout(sid);

    let pc;
    try {
      pc = makePc();
    } catch (e) {
      const detail = e?.message || 'PeerConnection creation failed';
      showToast(`WebRTC ${mode}: ${detail}`, 7000);
      send({
        type: mode === 'relay' ? 'relay_error' : 'webrtc_stop',
        sessionId: sid,
        detail,
        reason: 'browser-peer-create-failed',
      });
      closeLocalMedia({ keepSelection: true, keepSession: false, badge: 'failed' });
      scheduleRtcRetry();
      return;
    }
    state.pc = pc;
    state.lastFrameProgressAt = performance.now();
    const iceType = mode === 'source' ? 'webrtc_ice' : 'relay_ice';
    const answerType = mode === 'source' ? 'webrtc_answer' : 'relay_answer';

    pc.onicecandidate = ({ candidate }) => {
      if (!candidate || !isCurrentPc(pc, sid)) return;
      send({ type: iceType, sessionId: sid, candidate });
    };
    pc.ontrack = event => {
      if (!isCurrentPc(pc, sid)) return;
      state.remoteStream = event.streams[0] || new MediaStream([event.track]);
      els.video.srcObject = state.remoteStream;
      els.video.play().catch(() => {});
      els.placeholder.hidden = true;
      els.stats.hidden = false;
      setBadge(`${roleLabel()} · connected · waiting frames`);
      startStats(pc, sid);
      if (state.role === 'source') processPendingRelayStarts();
    };
    pc.onconnectionstatechange = () => {
      if (!isCurrentPc(pc, sid)) return;
      const s = pc.connectionState;
      send({ type: 'webrtc_state', sessionId: sid, state: s });
      if (s === 'connected') {
        state.retryCount = 0; cancelRtcRetry();
        if (state.mediaState !== 'live') setBadge(`${roleLabel()} · connected · waiting frames`);
        if (!state.statsTimer) startStats(pc, sid);
      } else if (s === 'connecting' || s === 'new') setBadge(`${roleLabel()} · connecting`);
      else if (s === 'disconnected') { setBadge(`${roleLabel()} · disconnected`); scheduleRtcRetry(6000); }
      else if (s === 'failed') { setBadge(`${roleLabel()} · failed`); scheduleRtcRetry(800); }
      else if (s === 'closed') setBadge(`${roleLabel()} · closed`);
      updateControls();
    };

    try {
      await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
      if (!isCurrentPc(pc, sid)) return closeSpecificPc(pc);
      await flushPendingRemoteIce(pc, sid);
      const answer = await pc.createAnswer();
      if (!isCurrentPc(pc, sid)) return closeSpecificPc(pc);
      await pc.setLocalDescription(answer);
      if (!isCurrentPc(pc, sid)) return closeSpecificPc(pc);
      if (!send({ type: answerType, sessionId: sid, sdp: pc.localDescription?.sdp || answer.sdp })) setBadge(`${roleLabel()} · signaling reconnect`);
    } catch (e) {
      if (!isCurrentPc(pc, sid)) return closeSpecificPc(pc);
      showToast(`WebRTC ${mode}: ${e.message}`, 7000);
      send({ type: mode === 'relay' ? 'relay_error' : 'webrtc_stop', sessionId: sid, detail: e.message, reason: 'browser-negotiation-failed' });
      closeLocalMedia({ keepSelection: true, keepSession: false, badge: 'failed' });
      scheduleRtcRetry();
    }
  }

  async function acceptMainRemoteIce(msg) {
    const sid = String(msg.sessionId || '');
    if (sid !== state.sessionId || !msg.candidate) return;
    const pc = state.pc;
    if (!pc || !pc.remoteDescription) {
      if (state.pendingRemoteIce.length >= 192) state.pendingRemoteIce.shift();
      state.pendingRemoteIce.push({ sid, candidate: msg.candidate });
      return;
    }
    try { await pc.addIceCandidate(msg.candidate); } catch { }
  }
  async function flushPendingRemoteIce(pc, sid) {
    if (!isCurrentPc(pc, sid) || !pc.remoteDescription) return;
    const pending = state.pendingRemoteIce; state.pendingRemoteIce = [];
    for (const item of pending) {
      if (item.sid !== sid || !isCurrentPc(pc, sid)) continue;
      try { await pc.addIceCandidate(item.candidate); } catch { }
    }
  }

  async function startOutboundRelay(msg) {
    if (state.role !== 'source' || !state.sessionId || msg.cameraId !== state.selected) return;
    const relaySid = String(msg.sessionId || '');
    if (!relaySid || relaySid === state.sessionId) return;
    if (!state.remoteStream || state.mediaState !== 'live') {
      state.pendingRelayStarts.set(relaySid, msg);
      return;
    }
    closeOutboundRelay(relaySid);
    let pc;
    const clonedTracks = [];
    try {
      pc = makePc();
      const relayStream = new MediaStream();
      for (const track of state.remoteStream.getVideoTracks()) {
        const clone = track.clone();
        clonedTracks.push(clone);
        relayStream.addTrack(clone);
        pc.addTrack(clone, relayStream);
      }
      if (!clonedTracks.length) throw new Error('source stream has no video track');
    } catch (e) {
      for (const track of clonedTracks) {
        try { track.stop(); } catch { }
      }
      if (pc) closeSpecificPc(pc);
      send({ type: 'relay_error', sessionId: relaySid, detail: e?.message || 'relay PeerConnection creation failed' });
      return;
    }
    const ctx = { pc, remoteReady: false, pendingIce: [], target: msg.target?.username || 'viewer', clonedTracks };
    state.relayOutbound.set(relaySid, ctx);
    pc.onicecandidate = ({ candidate }) => {
      if (!candidate || state.relayOutbound.get(relaySid)?.pc !== pc) return;
      send({ type: 'relay_ice', sessionId: relaySid, candidate });
    };
    pc.onconnectionstatechange = () => {
      if (state.relayOutbound.get(relaySid)?.pc !== pc) return;
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        send({ type: 'relay_error', sessionId: relaySid, detail: `relay host ${pc.connectionState}` });
        closeOutboundRelay(relaySid);
      }
    };
    try {
      const offer = await pc.createOffer({ offerToReceiveAudio: false, offerToReceiveVideo: false });
      if (state.relayOutbound.get(relaySid)?.pc !== pc) return closeSpecificPc(pc);
      await pc.setLocalDescription(offer);
      if (state.relayOutbound.get(relaySid)?.pc !== pc) return closeSpecificPc(pc);
      send({ type: 'relay_offer', sessionId: relaySid, sdp: pc.localDescription?.sdp || offer.sdp });
    } catch (e) {
      send({ type: 'relay_error', sessionId: relaySid, detail: e.message || 'relay offer failed' });
      closeOutboundRelay(relaySid);
    }
  }
  function processPendingRelayStarts() {
    if (state.role !== 'source' || state.mediaState !== 'live' || !state.remoteStream) return;
    const pending = [...state.pendingRelayStarts.values()];
    state.pendingRelayStarts.clear();
    for (const msg of pending) startOutboundRelay(msg);
  }
  async function acceptOutboundRelayAnswer(msg) {
    const sid = String(msg.sessionId || '');
    const ctx = state.relayOutbound.get(sid);
    if (!ctx || !msg.sdp) return;
    try {
      await ctx.pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
      if (state.relayOutbound.get(sid)?.pc !== ctx.pc) return;
      ctx.remoteReady = true;
      for (const candidate of ctx.pendingIce.splice(0)) {
        try { await ctx.pc.addIceCandidate(candidate); } catch { }
      }
    } catch (e) {
      send({ type: 'relay_error', sessionId: sid, detail: e.message || 'relay answer failed' });
      closeOutboundRelay(sid);
    }
  }
  async function acceptOutboundRelayIce(msg) {
    const sid = String(msg.sessionId || '');
    const ctx = state.relayOutbound.get(sid);
    if (!ctx || !msg.candidate) return;
    if (!ctx.remoteReady || !ctx.pc.remoteDescription) {
      if (ctx.pendingIce.length >= 128) ctx.pendingIce.shift();
      ctx.pendingIce.push(msg.candidate);
      return;
    }
    try { await ctx.pc.addIceCandidate(msg.candidate); } catch { }
  }
  function closeOutboundRelay(sid) {
    // relay_stop may arrive before SOURCE has received frames and created the outbound PC.
    // Remove the queued relay_start as well, otherwise a dead viewer is started later.
    state.pendingRelayStarts.delete(sid);
    const ctx = state.relayOutbound.get(sid);
    if (!ctx) return;
    state.relayOutbound.delete(sid);
    for (const track of ctx.clonedTracks || []) {
      try { track.stop(); } catch { }
    }
    try { ctx.pc.close(); } catch { }
  }
  function closeAllOutboundRelays() {
    for (const sid of [...state.relayOutbound.keys()]) closeOutboundRelay(sid);
    state.pendingRelayStarts.clear();
  }

  function makePc() {
    return new RTCPeerConnection({ iceServers: state.iceServers, bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require', iceCandidatePoolSize: 2 });
  }
  function validViewerId(v) { return /^[A-Za-z0-9_-]{12,96}$/.test(v || ''); }
  function newSessionId() { return crypto.randomUUID ? crypto.randomUUID().replaceAll('-', '') : `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}`; }
  function newViewerId() { return `viewer_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 14)}`; }
  function isCurrentPc(pc, sid) { return state.pc === pc && state.sessionId === sid; }
  function isCurrentSessionMessage(msg) { return Boolean(state.sessionId && msg.sessionId === state.sessionId); }
  function closeSpecificPc(pc) { try { pc.close(); } catch {} }
  function roleLabel() { return state.role === 'relay' ? 'RELAY' : state.role === 'source' ? 'SOURCE' : 'VIDEO'; }

  function armNegotiationTimeout(sid, delay = 25_000) {
    clearNegotiationTimer();
    if (!sid) return;
    state.negotiationTimer = setTimeout(() => {
      if (state.sessionId !== sid || state.mediaState === 'live' || state.manualStopped) return;
      showToast('WebRTC: таймаут подключения, повторяем');
      restartRtc(false);
    }, delay);
  }
  function clearNegotiationTimer() { clearTimeout(state.negotiationTimer); state.negotiationTimer = null; }
  function cancelRtcRetry() { clearTimeout(state.retryTimer); state.retryTimer = null; }
  function scheduleRtcRetry(delay = null) {
    cancelRtcRetry();
    if (state.manualStopped || !state.selected) return;
    const cameraId = state.selected;
    const wait = delay ?? Math.min(8000, 900 * (2 ** Math.min(state.retryCount, 4)));
    state.retryTimer = setTimeout(() => {
      state.retryTimer = null;
      if (state.manualStopped || state.selected !== cameraId || !selectedCameraReady()) return;
      if (state.ws?.readyState !== WebSocket.OPEN) return scheduleRtcRetry(1000);
      if (state.pc?.connectionState === 'connected' && state.mediaState === 'live') return;
      state.retryCount += 1;
      restartRtc(false);
    }, wait);
  }
  function maybeResumeSelected(delay = 250) {
    if (!state.selected || state.manualStopped || state.pc || state.sessionId || !selectedCameraReady()) return;
    scheduleRtcRetry(delay);
  }
  function restartRtc(userRequested) {
    const cameraId = state.selected;
    if (!cameraId) return;
    if (userRequested) { state.manualStopped = false; state.retryCount = 0; }
    cancelRtcRetry();
    stopCurrentSession(true, true);
    setTimeout(() => { if (state.selected === cameraId && !state.manualStopped) startRtc(cameraId); }, 200);
  }

  function resetMediaWatchdog() {
    state.lastFramesDecoded = null; state.lastFrameProgressAt = performance.now(); state.mediaState = 'waiting'; state.stallRestarting = false;
  }
  function closeMainPcKeepSession() {
    clearInterval(state.statsTimer); state.statsTimer = null;
    const pc = state.pc; state.pc = null; state.remoteStream = null; state.pendingRemoteIce = [];
    if (pc) try { pc.close(); } catch {}
    els.video.srcObject = null; els.placeholder.hidden = false; els.stats.hidden = true;
    resetMediaWatchdog();
    closeAllOutboundRelays();
  }
  function closeLocalMedia(options = {}) {
    clearNegotiationTimer(); clearInterval(state.statsTimer); state.statsTimer = null;
    state.lastBytes = null; state.lastAt = null; state.pendingRemoteIce = [];
    closeMainPcKeepSession();
    if (!options.keepSession) {
      if (state.sessionId) dropQueuedSignalsForSession(state.sessionId);
      state.sessionId = null; state.role = null;
    }
    setBadge(options.badge || 'idle');
    if (!options.keepSelection) state.selected = null;
    updateControls();
  }
  function stopCurrentSession(notify = true, keepSelection = true) {
    const sid = state.sessionId;
    // Clear stale queued ICE/SDP first, then send/queue STOP. If signaling is offline,
    // the STOP must survive until reconnect instead of being deleted by closeLocalMedia().
    closeLocalMedia({ keepSelection, keepSession: false, badge: 'idle' });
    if (notify && sid) send({ type: 'webrtc_stop', sessionId: sid, reason: 'viewer-stopped' });
  }
  function stopVideo() { state.manualStopped = true; cancelRtcRetry(); stopCurrentSession(true, true); setBadge('stopped'); }

  function startStats(pc, sid) {
    clearInterval(state.statsTimer);
    state.lastFrameProgressAt ??= performance.now();
    state.statsTimer = setInterval(async () => {
      if (!isCurrentPc(pc, sid)) return;
      try {
        const now = performance.now();
        const stats = await pc.getStats();
        if (!isCurrentPc(pc, sid)) return;
        let inbound = null, pair = null;
        stats.forEach(r => {
          if (r.type === 'inbound-rtp' && r.kind === 'video' && !r.isRemote) inbound = r;
          if (r.type === 'candidate-pair' && r.state === 'succeeded' && r.nominated) pair = r;
        });
        if (inbound) {
          els.resolution.textContent = `${inbound.frameWidth || '—'}×${inbound.frameHeight || '—'}`;
          els.fps.textContent = `${Math.round(inbound.framesPerSecond || 0)} FPS`;
          const bytes = inbound.bytesReceived || 0;
          if (state.lastBytes != null && state.lastAt != null) {
            const mbps = ((bytes - state.lastBytes) * 8) / ((now - state.lastAt) / 1000) / 1e6;
            els.bitrate.textContent = `${Math.max(0, mbps).toFixed(2)} Mbps`;
          }
          state.lastBytes = bytes; state.lastAt = now;
          const rtt = pair?.currentRoundTripTime;
          els.rtt.textContent = `RTT ${Number.isFinite(rtt) ? Math.round(rtt * 1000) : '—'} ms`;
          els.jitter.textContent = `Jitter ${Number.isFinite(inbound.jitter) ? Math.round(inbound.jitter * 1000) : '—'} ms`;
          const lost = Math.max(0, inbound.packetsLost || 0), recv = Math.max(0, inbound.packetsReceived || 0);
          els.loss.textContent = `Loss ${recv + lost > 0 ? ((lost / (recv + lost)) * 100).toFixed(2) : '0.00'}%`;
          const framesDecoded = Number(inbound.framesDecoded ?? inbound.framesReceived ?? 0);
          if (state.lastFramesDecoded == null || framesDecoded > state.lastFramesDecoded) {
            state.lastFramesDecoded = framesDecoded; state.lastFrameProgressAt = now;
            if (pc.connectionState === 'connected' && framesDecoded > 0 && state.mediaState !== 'live') {
              state.mediaState = 'live'; state.stallRestarting = false; state.retryCount = 0;
              clearNegotiationTimer(); cancelRtcRetry(); setBadge(`${roleLabel()} · live`);
              send({ type: 'webrtc_media', sessionId: sid, state: 'live' });
              if (state.role === 'source') processPendingRelayStarts();
            }
          }
        }
        if (pair) {
          const local = stats.get(pair.localCandidateId), remote = stats.get(pair.remoteCandidateId);
          const types = [local?.candidateType, remote?.candidateType].filter(Boolean);
          const path = types.includes('relay') ? 'TURN/relay' : (types.join('↔') || 'direct');
          els.route.textContent = `${state.role === 'relay' ? 'Via source · ' : ''}Route ${path}`;
        } else els.route.textContent = 'Route —';

        if (pc.connectionState === 'connected' && now - (state.lastFrameProgressAt || now) > 7000 && !state.stallRestarting) {
          state.stallRestarting = true; state.mediaState = 'stalled'; setBadge(`${roleLabel()} · stalled · reconnecting`);
          send({ type: 'webrtc_media', sessionId: sid, state: 'stalled' });
          showToast('Видео перестало получать кадры — переподключаем');
          setTimeout(() => { if (isCurrentPc(pc, sid) && state.mediaState === 'stalled') restartRtc(false); }, 600);
        }
      } catch { }
    }, 1000);
  }

  function setBadge(s) { els.badge.textContent = `Video: ${s}`; els.badge.className = `status-badge ${String(s).includes('live') ? 'online' : 'offline'}`; }
  function showToast(text, duration = 4000) { els.toast.textContent = text; els.toast.hidden = false; clearTimeout(showToast.timer); showToast.timer = setTimeout(() => { els.toast.hidden = true; }, duration); }
  function escapeHtml(s) { return String(s).replace(/[&<>'"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[c])); }

  els.reconnect?.addEventListener('click', () => restartRtc(true));
  els.stop?.addEventListener('click', stopVideo);
  els.servoLeft?.addEventListener('click', () => sendServo('left'));
  els.servoRight?.addEventListener('click', () => sendServo('right'));
  els.servoAngleInput?.addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); sendAbsoluteServoAngle(); }
  });
  // Arrow keys drive the servo as long as the operator is not typing in a field.
  document.addEventListener('keydown', event => {
    if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) return;
    const tag = (event.target?.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'select' || tag === 'textarea' || event.target?.isContentEditable) return;
    if (!state.selected || els.servoLeft?.disabled) return;
    // event.code is layout-independent: Q/E work on a Cyrillic layout (Й/У) too.
    const code = event.code;
    if (code === 'KeyQ' || event.key === 'ArrowLeft') { event.preventDefault(); sendServo('left'); }
    else if (code === 'KeyE' || event.key === 'ArrowRight') { event.preventDefault(); sendServo('right'); }
  });
  els.logout.addEventListener('click', async () => {
    state.manualStopped = true; cancelRtcRetry(); stopCurrentSession(true, false);
    await fetch('/api/logout', { method: 'POST' }); location.assign('/login');
  });
  window.addEventListener('beforeunload', () => {
    const sid = state.sessionId;
    if (sid && state.ws?.readyState === WebSocket.OPEN) {
      try { state.ws.send(JSON.stringify({ type: 'webrtc_stop', sessionId: sid, reason: 'viewer-page-closed' })); } catch { }
    }
  });
  boot();
})();
