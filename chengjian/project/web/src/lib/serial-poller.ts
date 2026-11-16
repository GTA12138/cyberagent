// One request at a time. Refresh invalidates an in-flight response and schedules
// one fresh request; stopping also suppresses responses from transports ignoring abort.
export function startSerialPoll<T>(options: {
  fetch: (signal: AbortSignal) => Promise<T>;
  receive: (value: T) => void;
  error: (error: unknown) => void;
  interval: number;
}) {
  let stopped = false;
  let running = false;
  let generation = 0;
  let pending = false;
  let controller: AbortController | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const run = async () => {
    if (stopped || running) return;
    running = true;
    pending = false;
    const current = generation;
    controller = new AbortController();
    try {
      const value = await options.fetch(controller.signal);
      if (!stopped && current === generation) options.receive(value);
    } catch (error) {
      if (!stopped && current === generation) options.error(error);
    } finally {
      running = false;
      if (!stopped) {
        if (pending) void run();
        else timer = setTimeout(() => void run(), options.interval);
      }
    }
  };
  void run();
  return {
    refresh() {
      if (stopped) return;
      generation++;
      clearTimeout(timer);
      pending = true;
      if (running) controller?.abort();
      else void run();
    },
    stop() {
      stopped = true;
      generation++;
      clearTimeout(timer);
      controller?.abort();
    },
  };
}
