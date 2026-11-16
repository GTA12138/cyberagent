"use client";

import * as React from "react";
import { http } from "@/lib/api";
import { startSerialPoll } from "@/lib/serial-poller";
import type { Task, TaskCategory } from "@/lib/types";

export function useTasksData() {
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [overviewLoaded, setOverviewLoaded] = React.useState(false);
  const [overviewError, setOverviewError] = React.useState("");
  const [categories, setCategories] = React.useState<TaskCategory[]>([]);
  const [categoriesLoaded, setCategoriesLoaded] = React.useState(false);
  const lastRef = React.useRef("");
  const taskPoll = React.useRef<ReturnType<typeof startSerialPoll> | null>(null);
  const categoryPoll = React.useRef<ReturnType<typeof startSerialPoll> | null>(null);
  React.useEffect(() => {
    const list = startSerialPoll({
      fetch: (signal) => http<{ tasks: Task[]; active: string }>("/tasks", { signal }),
      receive: (r) => {
        setOverviewLoaded(true);
        setOverviewError("");
        const next = (r.tasks ?? []).map((t) => (t.id === r.active ? { ...t, active: true } : t));
        const signature = JSON.stringify(next);
        if (lastRef.current !== signature) {
          lastRef.current = signature;
          setTasks(next);
        }
      },
      error: (error) => setOverviewError(`任务读取失败：${(error as Error).message}；稍后自动重试`),
      interval: 10000,
    });
    const groups = startSerialPoll({
      fetch: (signal) => http<{ categories: TaskCategory[] }>("/task-categories", { signal }),
      receive: (r) => {
        setCategories(r.categories ?? []);
        setCategoriesLoaded(true);
      },
      error: () => {},
      interval: 30000,
    });
    taskPoll.current = list;
    categoryPoll.current = groups;
    return () => {
      list.stop();
      groups.stop();
      taskPoll.current = null;
      categoryPoll.current = null;
    };
  }, []);
  const load = React.useCallback(() => {
    lastRef.current = "";
    taskPoll.current?.refresh();
  }, []);
  const loadCategories = React.useCallback(() => categoryPoll.current?.refresh(), []);
  const refreshCategoriesAndTasks = React.useCallback(() => {
    lastRef.current = "";
    load();
    loadCategories();
  }, [load, loadCategories]);
  const applyTaskCategoryMove = React.useCallback(
    (taskID: string, category: TaskCategory | null) => {
      setTasks((current) => {
        const next = current.map((task) =>
          task.id === taskID ? { ...task, category_id: category?.id, category_name: category?.name } : task,
        );
        lastRef.current = JSON.stringify(next);
        return next;
      });
      loadCategories();
      load();
    },
    [load, loadCategories],
  );
  return {
    tasks,
    overviewLoaded,
    overviewError,
    categories,
    categoriesLoaded,
    load,
    applyTaskCategoryMove,
    refreshCategoriesAndTasks,
  };
}
