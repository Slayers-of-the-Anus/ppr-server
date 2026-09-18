(() => {
  const $ = id => document.getElementById(id);
  const els = {
    preview: $('preview'), secure: $('secure'), camera: $('cameraState'), socket: $('socketState'), rtc: $('rtcState'),
    id: $('cameraId'), name: $('cameraName'), start: $('start'), stop: $('stop'), error: $('error'),
  };
  const state = { stream: null, ws: null, sessions: new Map(), iceServers: [] };
  els.secure.textContent = `secure: ${window.isSecureContext ? 'yes' : 'NO'}`;

  function setError(text = '') { els.error.textContent = text; }
  function send(obj) {
    if (state.ws?.readyState !== WebSocket.OPEN) return false;
    try { state.ws.send(JSON.stringify(obj)); return true; } catch { return false; }
  }
  function wsUrl() {
    const p = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${p}//${location.host}/ws/camera?cameraId=${encodeURIComponent(els.id.value.trim())}&name=${encodeURIComponent(els.name.value.trim())}`;
  }

  async function start() {
    setError('');
    if (!navigator.mediaDevices?.getUserMedia) return setError('Камера браузера недоступна. Откройте страницу в secure context: HTTPS или localhost.');
    els.start.disabled = true;
    try {
      state.stream = await navigator.mediaDevices.getUserMedia({
        audio: false,
        video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280, max: 1280 }, height: { ideal: 720, max: 720 }, frameRate: { ideal: 30, max: 30 } },
      });
      els.preview.srcObject = state.stream;
      await els.preview.play().catch(() => {});
      const track = state.stream.getVideoTracks()[0];
      const settings = track.getSettings();
      els.camera.textContent = `camera: ${settings.width || '?'}×${settings.height || '?'} @ ${settings.frameRate ? Math.round(settings.frameRate) : '?'}fps`;
      connectSocket(settings);
      els.stop.disabled = false;
    } catch (e) {
      els.start.disabled = false;
      setError(`Camera error: ${e.message}`);
    }
  }

  function connectSocket(settings) {
    const ws = new WebSocket(wsUrl());
    state.ws = ws;
    ws.onopen = () => {
      if (state.ws !== ws) return;
      els.socket.textContent = 'server: connected';
      sendCameraStatus(settings, true);
    };
    ws.onerror = () => { if (state.ws === ws) els.socket.textContent = 'server: error'; };
    ws.onclose = () => {
      if (state.ws !== ws) return;
      state.ws = null;
      els.socket.textContent = 'server: off';
      // Keep established peer connections alive briefly just like the native client.
    };
    ws.onmessage = async ev => {
      if (state.ws !== ws) return;
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      if (msg.type === 'camera_ready' || msg.type === 'robot_ready') {
        state.iceServers = msg.iceServers || state.iceServers;
        sendCameraStatus(settings, true);
      } else if (msg.type === 'viewer_list') {
        els.rtc.textContent = `viewers: ${msg.count || 0}/${msg.max || 4}`;
      } else if (msg.type === 'webrtc_start') {
        await makeOffer(msg);
      } else if (msg.type === 'webrtc_answer') {
        await acceptAnswer(msg);
      } else if (msg.type === 'webrtc_ice') {
        await acceptRemoteIce(msg);
      } else if (msg.type === 'webrtc_stop') {
        closeSession(String(msg.sessionId || ''), false);
      }
    };
  }

  function sendCameraStatus(settings, available) {
    send({
      type: 'camera_status', available,
      width: settings?.width || 1280, height: settings?.height || 720, fps: Math.round(settings?.frameRate || 30),
    });
  }

  async function makeOffer(msg) {
    const sid = String(msg.sessionId || '');
    if (!sid || !state.stream || state.sessions.has(sid)) return;
    if (state.sessions.size >= 1) {
      send({ type: 'webrtc_error', sessionId: sid, code: 'SOURCE_BUSY', detail: 'v0.7 camera sender uses one upstream; extra viewers are browser-relayed' });
      return;
    }

    const ctx = { pc: null, pendingRemoteIce: [] };
    const pc = new RTCPeerConnection({ iceServers: msg.iceServers || state.iceServers, bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' });
    ctx.pc = pc;
    state.sessions.set(sid, ctx);
    const track = state.stream.getVideoTracks()[0];
    if (!track) return closeSession(sid, false);
    const transceiver = pc.addTransceiver(track, { direction: 'sendonly', streams: [state.stream] });
    if (transceiver.setCodecPreferences && RTCRtpSender.getCapabilities) {
      const codecs = RTCRtpSender.getCapabilities('video')?.codecs || [];
      const preferred = [...codecs.filter(c => /H264/i.test(c.mimeType)), ...codecs.filter(c => /VP8/i.test(c.mimeType)), ...codecs.filter(c => !/H264|VP8/i.test(c.mimeType))];
      try { transceiver.setCodecPreferences(preferred); } catch {}
    }

    pc.onicecandidate = e => {
      if (e.candidate && isCurrent(sid, pc)) send({ type: 'webrtc_ice', sessionId: sid, candidate: e.candidate });
    };
    pc.onconnectionstatechange = () => {
      if (!isCurrent(sid, pc)) return;
      send({ type: 'webrtc_state', sessionId: sid, state: pc.connectionState });
    };

    try {
      const offer = await pc.createOffer({ offerToReceiveAudio: false, offerToReceiveVideo: false });
      if (!isCurrent(sid, pc)) return;
      await pc.setLocalDescription(offer);
      if (!isCurrent(sid, pc)) return;
      send({ type: 'webrtc_offer', sessionId: sid, sdp: pc.localDescription?.sdp || offer.sdp });
    } catch (e) {
      if (isCurrent(sid, pc)) {
        send({ type: 'webrtc_error', sessionId: sid, code: 'OFFER_FAILED', detail: e.message });
        setError(`Offer: ${e.message}`);
      }
    }
  }

  async function acceptAnswer(msg) {
    const sid = String(msg.sessionId || '');
    const ctx = state.sessions.get(sid);
    if (!ctx?.pc || !msg.sdp) return;
    const pc = ctx.pc;
    try {
      await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
      if (!isCurrent(sid, pc)) return;
      const pending = ctx.pendingRemoteIce.splice(0);
      for (const candidate of pending) {
        if (!isCurrent(sid, pc)) return;
        try { await pc.addIceCandidate(candidate); } catch {}
      }
    } catch (e) {
      setError(`Answer: ${e.message}`);
    }
  }

  async function acceptRemoteIce(msg) {
    const sid = String(msg.sessionId || '');
    const ctx = state.sessions.get(sid);
    if (!ctx || !msg.candidate) return;
    const pc = ctx.pc;
    if (!pc.remoteDescription) {
      if (ctx.pendingRemoteIce.length >= 192) ctx.pendingRemoteIce.shift();
      ctx.pendingRemoteIce.push(msg.candidate);
      return;
    }
    try { await pc.addIceCandidate(msg.candidate); } catch {}
  }

  function isCurrent(sid, pc) { return state.sessions.get(sid)?.pc === pc; }

  function closeSession(sid, notify = false) {
    const ctx = state.sessions.get(sid);
    if (!ctx) return;
    if (notify) send({ type: 'webrtc_stop', sessionId: sid, reason: 'sender-stop' });
    state.sessions.delete(sid);
    try { ctx.pc.close(); } catch {}
  }

  function closeAll(notify = false) {
    for (const sid of [...state.sessions.keys()]) closeSession(sid, notify);
  }

  function stop() {
    closeAll(true);
    const track = state.stream?.getVideoTracks()[0];
    sendCameraStatus(track?.getSettings?.(), false);
    try { state.ws?.send(JSON.stringify({ type: 'camera_disconnect' })); } catch {}
    state.ws?.close(1000, 'client-disconnect');
    state.ws = null;
    state.stream?.getTracks().forEach(t => t.stop());
    state.stream = null;
    els.preview.srcObject = null;
    els.camera.textContent = 'camera: off';
    els.socket.textContent = 'server: off';
    els.rtc.textContent = 'viewers: 0/4';
    els.start.disabled = false;
    els.stop.disabled = true;
  }

  els.start.onclick = start;
  els.stop.onclick = stop;
  addEventListener('beforeunload', stop);
})();
