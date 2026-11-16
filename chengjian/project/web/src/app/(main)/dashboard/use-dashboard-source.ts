"use client";

import { useEffect, useState } from "react";

// Each source retries independently; a failed optional endpoint must not hide risk data.
export function useDashboardSource<T>(load: () => Promise<T>, interval: number, revision: number) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState(false);
  const [updated, setUpdated] = useState<Date | null>(null);
  const [loading, setLoading] = useState(true);

  // revision explicitly invalidates this polling generation for manual refresh.
  useEffect(() => {
    void revision;
    let alive = true;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      setLoading(true);
      try {
        const value = await load();
        if (alive) {
          setData(value);
          setUpdated(new Date());
          setError(false);
        }
      } catch {
        if (alive) setError(true);
      } finally {
        if (alive) {
          setLoading(false);
          timer = setTimeout(() => void poll(), interval);
        }
      }
    }
    void poll();
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [load, interval, revision]);

  return { data, error, updated, loading };
}
