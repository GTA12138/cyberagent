export interface ActivityStreamSource {
  onopen: ((event: Event) => void) | null;
  onerror: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  close(): void;
}

// Tickets are single use: close on every transport error and obtain a fresh URL.
// Advance only after the consumer accepts the frame; reconnect from that cursor.
export function startActivityStream<T extends { seq: number }>(options: {
  cursor: number;
  url: (cursor: number) => Promise<string>;
  connect: (url: string) => ActivityStreamSource;
  parse: (data: unknown) => T;
  receive: (item: T) => void;
  live: (live: boolean) => void;
  retryMs?: number;
}) {
  let stopped = false;
  let cursor = options.cursor;
  let source: ActivityStreamSource | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let failures = 0;
  const retry = () => {
    if (stopped || timer !== undefined) return;
    options.live(false);
    source?.close();
    source = undefined;
    const delay = Math.min((options.retryMs ?? 1000) * 2 ** Math.min(failures++, 5), 30000);
    timer = setTimeout(() => {
      timer = undefined;
      void open();
    }, delay);
  };
  const open = async () => {
    try {
      const url = await options.url(cursor);
      if (stopped) return;
      const current = options.connect(url);
      source = current;
      current.onopen = () => {
        if (stopped || source !== current) return;
        failures = 0;
        options.live(true);
      };
      current.onerror = () => {
        if (source === current) retry();
      };
      current.onmessage = (event) => {
        if (stopped || source !== current) return;
        try {
          const item = options.parse(JSON.parse(event.data));
          if (!Number.isSafeInteger(item.seq) || item.seq <= cursor) return;
          options.receive(item);
          cursor = item.seq;
        } catch {
          // A bad frame must not advance the recovery cursor.
          retry();
        }
      };
    } catch {
      retry();
    }
  };
  void open();
  return {
    stop() {
      stopped = true;
      clearTimeout(timer);
      source?.close();
      source = undefined;
    },
  };
}
