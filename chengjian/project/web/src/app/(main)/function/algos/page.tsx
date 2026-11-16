"use client";

// 预置算法库管理页：签名/滑块/加解密算法的登记、试跑、启停。
// 任务启动前把逆向成果传进来，任务里 agent 用 apply_algo 直接调用，
// 不再现场逆向（省 token、成果跨任务复用）。
import * as React from "react";
import { http } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { toast } from "sonner";

interface AlgoRow {
  id: number;
  name: string;
  kind: string;
  match_host?: string;
  match_path?: string;
  description?: string;
  enabled: boolean;
  usage_count: number;
}

const KIND_LABEL: Record<string, string> = {
  sign: "签名",
  slider: "滑块",
  encrypt: "加密",
  decrypt: "解密",
  other: "其他",
};

const SAMPLE_SCRIPT = `import sys, json
p = json.loads(sys.stdin.readline())
# TODO: 把逆向出的算法放进来（纯计算，不发网络请求，15s 内完成）
print(json.dumps({"sign": "todo-" + str(p.get("ts", 0))}))`;

export default function AlgosPage() {
  const [rows, setRows] = React.useState<AlgoRow[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [open, setOpen] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [form, setForm] = React.useState({
    name: "",
    kind: "sign",
    match_host: "",
    match_path: "",
    description: "",
    script: SAMPLE_SCRIPT,
    verify_input: "",
  });
  const [testInput, setTestInput] = React.useState("{}");
  const [testing, setTesting] = React.useState<number | null>(null);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const r = await http<{ items: AlgoRow[] }>("/algorithms");
      setRows(r.items ?? []);
    } catch (e) {
      toast.error("读取失败：" + (e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (!form.name.trim() || !form.description.trim() || !form.script.trim()) {
      toast.error("名称 / 说明 / 脚本必填");
      return;
    }
    setSaving(true);
    try {
      const body: Record<string, unknown> = {
        name: form.name.trim(),
        kind: form.kind,
        match_host: form.match_host.trim(),
        match_path: form.match_path.trim(),
        description: form.description.trim(),
        script: form.script,
      };
      if (form.verify_input.trim()) body.verify_input = JSON.parse(form.verify_input);
      await http("/algorithms", { method: "POST", body: JSON.stringify(body) });
      toast.success("已保存" + (form.verify_input.trim() ? "（实跑验证通过）" : ""));
      setOpen(false);
      setForm({ name: "", kind: "sign", match_host: "", match_path: "", description: "", script: SAMPLE_SCRIPT, verify_input: "" });
      await load();
    } catch (e) {
      toast.error("保存失败：" + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function test(id: number) {
    setTesting(id);
    try {
      const input = JSON.parse(testInput || "{}");
      const r = await http<{ output: unknown }>(`/algorithms/${id}/test`, {
        method: "POST",
        body: JSON.stringify({ input }),
      });
      toast.success("输出：" + JSON.stringify(r.output));
    } catch (e) {
      toast.error("试跑失败：" + (e as Error).message);
    } finally {
      setTesting(null);
    }
  }

  async function toggle(row: AlgoRow) {
    try {
      await http(`/algorithms/${row.id}/${row.enabled ? "disable" : "enable"}`, {
        method: row.enabled ? "DELETE" : "POST",
      });
      await load();
    } catch (e) {
      toast.error("操作失败：" + (e as Error).message);
    }
  }

  return (
    <div className="space-y-4 p-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">算法库</h1>
          <p className="text-muted-foreground text-sm">
            预置签名 / 滑块 / 加解密算法：任务启动前登记，任务内 agent 经 apply_algo 直接调用（本地计算、不烧逆向 token）；http_fetch / http_replay 命中匹配条件会自动提示。
          </p>
        </div>
        <Button onClick={() => setOpen(true)}>登记算法</Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>类别</TableHead>
                <TableHead>匹配</TableHead>
                <TableHead>说明</TableHead>
                <TableHead>调用次数</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && (
                <TableRow>
                  <TableCell colSpan={7} className="text-muted-foreground py-8 text-center">
                    加载中…
                  </TableCell>
                </TableRow>
              )}
              {!loading && rows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={7} className="text-muted-foreground py-8 text-center">
                    还没有算法。点「登记算法」把逆向成果沉淀进来（脚本契约：stdin JSON → stdout JSON）。
                  </TableCell>
                </TableRow>
              )}
              {rows.map((row) => (
                <TableRow key={row.id}>
                  <TableCell className="font-medium">{row.name}</TableCell>
                  <TableCell>
                    <Badge variant="outline">{KIND_LABEL[row.kind] ?? row.kind}</Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground text-xs">
                    {[row.match_host, row.match_path].filter(Boolean).join(" · ") || "—"}
                  </TableCell>
                  <TableCell className="max-w-[320px] truncate text-sm" title={row.description}>
                    {row.description}
                  </TableCell>
                  <TableCell>{row.usage_count}</TableCell>
                  <TableCell>
                    <Badge variant={row.enabled ? "default" : "secondary"}>
                      {row.enabled ? "启用" : "停用"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-2">
                      <Button size="sm" variant="outline" disabled={testing === row.id} onClick={() => void test(row.id)}>
                        {testing === row.id ? "试跑中…" : "试跑"}
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => void toggle(row)}>
                        {row.enabled ? "停用" : "启用"}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">试跑入参（对所有算法通用）</CardTitle>
        </CardHeader>
        <CardContent>
          <Textarea
            value={testInput}
            onChange={(e) => setTestInput(e.target.value)}
            placeholder='例如 {"ts":1698000000,"body":"abc","gap":87}'
            className="font-mono text-xs"
            rows={3}
          />
        </CardContent>
      </Card>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>登记算法</DialogTitle>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-1">
                <Label>名称（英文短名，如 acme_sign_v1）</Label>
                <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
              </div>
              <div className="grid gap-1">
                <Label>类别</Label>
                <NativeSelect value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
                  {Object.entries(KIND_LABEL).map(([k, v]) => (
                    <option key={k} value={k}>
                      {v}
                    </option>
                  ))}
                </NativeSelect>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-1">
                <Label>匹配 host 子串（命中提示用，可空）</Label>
                <Input value={form.match_host} onChange={(e) => setForm({ ...form, match_host: e.target.value })} placeholder="api.acme.com" />
              </div>
              <div className="grid gap-1">
                <Label>匹配 path 子串（可空）</Label>
                <Input value={form.match_path} onChange={(e) => setForm({ ...form, match_path: e.target.value })} placeholder="/api/order" />
              </div>
            </div>
            <div className="grid gap-1">
              <Label>说明（入参字段含义——agent 靠它调用）</Label>
              <Input value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} placeholder='入参 ts(秒级时间戳)、body(原文)；输出 sign 字段' />
            </div>
            <div className="grid gap-1">
              <Label>Python 脚本（stdin 一行 JSON → stdout 一行 JSON；纯计算、无网络、15s 内）</Label>
              <Textarea value={form.script} onChange={(e) => setForm({ ...form, script: e.target.value })} className="font-mono text-xs" rows={10} />
            </div>
            <div className="grid gap-1">
              <Label>保存时实跑验证入参（强烈建议填，契约跑不通会拒绝保存）</Label>
              <Input value={form.verify_input} onChange={(e) => setForm({ ...form, verify_input: e.target.value })} placeholder='{"ts":1698000000,"body":"abc"}' className="font-mono text-xs" />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button onClick={() => void save()} disabled={saving}>
              {saving ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
