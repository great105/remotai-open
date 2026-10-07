/** Framework-independent microphone adapter for the existing apk/src client. */
export interface PhoneRecording {
  stop(): Promise<File>;
  cancel(): void;
}

export interface PhoneAudioAPI<T> {
  upload(file: File, signal?: AbortSignal): Promise<{ path: string }>;
  transcribe(params: { path: string; task_id?: string }, signal?: AbortSignal): Promise<T>;
}

function aborted(): DOMException {
  return new DOMException("Запись отменена", "AbortError");
}

export async function startPhoneRecording(options: {
  signal?: AbortSignal;
  mediaDevices?: Pick<MediaDevices, "getUserMedia">;
  Recorder?: typeof MediaRecorder;
} = {}): Promise<PhoneRecording> {
  const devices = options.mediaDevices ?? globalThis.navigator?.mediaDevices;
  const Recorder = options.Recorder ?? globalThis.MediaRecorder;
  const signal = options.signal;
  if (signal?.aborted) throw aborted();
  if (!devices?.getUserMedia || !Recorder) {
    throw new Error("Микрофон недоступен в этой среде. Выберите готовый аудиофайл.");
  }

  // A permission dialog can stay pending indefinitely. After cancellation,
  // a stream granted later must still be closed.
  const permission = devices.getUserMedia({ audio: true, video: false });
  const closedStreams = new WeakSet<MediaStream>();
  const closeStream = (stream: MediaStream) => {
    if (closedStreams.has(stream)) return;
    closedStreams.add(stream);
    stream.getTracks().forEach(track => track.stop());
  };
  void permission.then(stream => { if (signal?.aborted) closeStream(stream); }, () => {});
  let permissionAbort: (() => void) | undefined;
  const abortWait = new Promise<never>((_, reject) => {
    permissionAbort = () => reject(aborted());
    signal?.addEventListener("abort", permissionAbort, { once: true });
    if (signal?.aborted) permissionAbort();
  });
  let stream: MediaStream;
  try {
    stream = await Promise.race([permission, abortWait]);
  } finally {
    if (permissionAbort) signal?.removeEventListener("abort", permissionAbort);
  }
  if (signal?.aborted) { closeStream(stream); throw aborted(); }

  let recorder: MediaRecorder;
  try {
    const mime = ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/mp4"]
      .find(candidate => Recorder.isTypeSupported(candidate));
    recorder = new Recorder(stream, mime ? { mimeType: mime } : undefined);
  } catch (error) {
    closeStream(stream);
    throw error;
  }
  const chunks: Blob[] = [];
  let finished = false;
  let released = false;
  let resolveFile!: (file: File) => void;
  let rejectFile!: (error: unknown) => void;
  const complete = new Promise<File>((resolve, reject) => { resolveFile = resolve; rejectFile = reject; });
  void complete.catch(() => {}); // a native error can precede the Stop click
  const release = () => {
    if (released) return;
    released = true;
    signal?.removeEventListener("abort", cancel);
    closeStream(stream);
  };
  const cancel = () => {
    if (finished) return;
    finished = true;
    rejectFile(aborted());
    try { if (recorder.state !== "inactive") recorder.stop(); } catch { /* already failed */ }
    release();
  };
  recorder.addEventListener("dataavailable", event => {
    if (!finished && event.data.size) chunks.push(event.data);
  });
  recorder.addEventListener("error", event => {
    if (finished) return;
    finished = true;
    rejectFile((event as Event & { error?: Error }).error ?? new Error("Не удалось записать звук"));
    release();
  });
  recorder.addEventListener("stop", () => {
    if (finished) return;
    finished = true;
    release();
    const mime = recorder.mimeType || chunks[0]?.type || "application/octet-stream";
    const blob = new Blob(chunks, { type: mime });
    if (!blob.size) { rejectFile(new Error("Запись пустая")); return; }
    const extension = mime.includes("ogg") ? "ogg" : mime.includes("mp4") ? "m4a" : mime.includes("webm") ? "webm" : "bin";
    resolveFile(new File([blob], `voice-${Date.now()}.${extension}`, { type: mime }));
  });
  try {
    recorder.start();
    signal?.addEventListener("abort", cancel, { once: true });
    if (signal?.aborted) cancel();
  } catch (error) {
    finished = true;
    release();
    throw error;
  }
  let stopRequested = false;
  return {
    stop() {
      if (!finished && !stopRequested) {
        stopRequested = true;
        try { recorder.stop(); } catch (error) { finished = true; release(); rejectFile(error); }
      }
      return complete;
    },
    cancel,
  };
}

export async function transcribePhoneRecording<T>(
  recording: PhoneRecording, api: PhoneAudioAPI<T>, taskID?: string, signal?: AbortSignal,
): Promise<T> {
  const file = await recording.stop();
  if (signal?.aborted) throw aborted();
  const uploaded = await api.upload(file, signal);
  if (signal?.aborted) throw aborted();
  if (!uploaded.path) throw new Error("Загрузка ещё не завершена: сервер не вернул путь файла");
  return api.transcribe({ path: uploaded.path, ...(taskID ? { task_id: taskID } : {}) }, signal);
}
