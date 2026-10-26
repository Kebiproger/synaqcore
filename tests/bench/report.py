#!/usr/bin/env python3
"""Обработка результатов k6 для tests/bench/run.sh (только стандартная библиотека).

Подкоманды:
  verdict STEP_JSON RATE DURATION_S   печатает OK или SATURATED:<причина> (для остановки лестницы)
  aggregate RAWDIR OUT_JSON           собирает итоговый JSON и печатает таблицу в Markdown

RAWDIR содержит для каждого запуска k6 (label):
  <label>.json        k6 --summary-export
  <label>.meta.json   {"label","kind":"ladder|knee|half","rate","duration_s"}
  <label>.stats.tsv   выборки docker stats: время, контейнер, CPU%, память (байты)
и общие файлы run-info.json, integrity.json.
"""
import json
import sys
from pathlib import Path

P99_LIMIT_MS = 250.0     # порог "колена"
ERR_LIMIT = 0.01         # доля ошибок
DROP_LIMIT = 0.01        # доля dropped_iterations от плана
STOP_P99_MS = 1000.0     # лестница останавливается, если p99 выше


def _m(metrics, name):
    return metrics.get(name, {})


def parse_step(path, rate, duration_s):
    """Достаёт из summary-export метрики сценария measure."""
    data = json.loads(Path(path).read_text())
    m = data.get("metrics", {})
    dur = _m(m, "http_req_duration{scenario:measure}")
    reqs = _m(m, "http_reqs{scenario:measure}").get("count", 0)
    failed = _m(m, "http_req_failed{scenario:measure}")
    dropped = _m(m, "dropped_iterations{scenario:measure}").get("count", 0)
    planned = rate * duration_s
    fails, passes = failed.get("passes", 0), failed.get("fails", 0)
    total = fails + passes
    out = {
        "target_rps": rate,
        "achieved_rps": round(reqs / duration_s, 1) if duration_s else 0,
        "requests": reqs,
        "p50_ms": dur.get("med"),
        "p90_ms": dur.get("p(90)"),
        "p95_ms": dur.get("p(95)"),
        "p99_ms": dur.get("p(99)"),
        "max_ms": dur.get("max"),
        "avg_ms": dur.get("avg"),
        "error_rate": round(fails / total, 5) if total else None,
        "dropped_iterations": dropped,
        "dropped_share": round(dropped / planned, 5) if planned else 0,
    }
    for name in ("variants", "tracker"):
        d = _m(m, f"http_req_duration{{scenario:measure,name:{name}}}")
        if d:
            out[f"{name}_ms"] = {k: d.get(v) for k, v in
                                 (("p50", "med"), ("p95", "p(95)"), ("p99", "p(99)"))}
            out[f"{name}_requests"] = _m(m, f"http_reqs{{scenario:measure,name:{name}}}").get("count", 0)
    for kind in ("ok", "429", "503", "other"):
        out[f"trk_{kind}"] = _m(m, f"trk_{kind}_measure").get("count", 0)
        out[f"trk_{kind}_all"] = (_m(m, f"trk_{kind}_measure").get("count", 0)
                                  + _m(m, f"trk_{kind}_warmup").get("count", 0))
    return out


def judge(s):
    """None, если ступень "здорова" (p99<250 мс, ошибки<1%, нет заметных дропов), иначе причина."""
    reasons = []
    if s["p99_ms"] is None:
        return "нет данных"
    if s["p99_ms"] >= P99_LIMIT_MS:
        reasons.append(f"p99={s['p99_ms']:.0f}мс")
    if s["error_rate"] is not None and s["error_rate"] > ERR_LIMIT:
        reasons.append(f"ошибки={s['error_rate']:.2%}")
    if s["dropped_share"] > DROP_LIMIT:
        reasons.append(f"dropped={s['dropped_share']:.1%}")
    return ", ".join(reasons) or None


def cmd_verdict(step_json, rate, duration_s):
    s = parse_step(step_json, int(rate), float(duration_s))
    hard = []
    if s["p99_ms"] is None:
        hard.append("нет данных")
    else:
        if s["p99_ms"] > STOP_P99_MS:
            hard.append(f"p99={s['p99_ms']:.0f}мс>1с")
        if s["error_rate"] is not None and s["error_rate"] > ERR_LIMIT:
            hard.append(f"ошибки={s['error_rate']:.2%}")
        if s["dropped_share"] > DROP_LIMIT:
            hard.append(f"dropped={s['dropped_share']:.1%}")
    print("SATURATED:" + ", ".join(hard) if hard else "OK")


def parse_mem(txt):
    num = float("".join(c for c in txt.split("/")[0] if c.isdigit() or c == "."))
    unit = txt.split("/")[0].strip().lstrip("0123456789.")
    mult = {"B": 1, "KiB": 2**10, "MiB": 2**20, "GiB": 2**30,
            "kB": 1e3, "MB": 1e6, "GB": 1e9}.get(unit, 1)
    return num * mult


def parse_stats(path):
    """Пик CPU% и памяти по контейнерам из <label>.stats.tsv (контейнер, CPU%, память)."""
    peak = {}
    p = Path(path)
    if not p.exists():
        return peak
    for line in p.read_text().splitlines():
        parts = line.split("\t")
        if len(parts) < 4:
            continue
        _, name, cpu, mem = parts[:4]
        try:
            cpu_v = float(cpu.rstrip("%"))
            mem_v = parse_mem(mem)
        except ValueError:
            continue
        cur = peak.setdefault(name, {"cpu_pct_max": 0.0, "mem_mib_max": 0.0, "_sum": 0.0, "_n": 0})
        cur["_sum"] += cpu_v
        cur["_n"] += 1
        cur["cpu_pct_avg"] = round(cur["_sum"] / cur["_n"], 1)
        cur["cpu_pct_max"] = max(cur["cpu_pct_max"], cpu_v)
        cur["mem_mib_max"] = max(cur["mem_mib_max"], round(mem_v / 2**20, 1))
    for v in peak.values():
        v.pop("_sum", None)
        v.pop("_n", None)
    return peak


def fmt(v, nd=1):
    return "-" if v is None else f"{v:.{nd}f}"


def cmd_aggregate(rawdir, out_json):
    raw = Path(rawdir)
    runs = []
    for meta_path in sorted(raw.glob("*.meta.json")):
        meta = json.loads(meta_path.read_text())
        jpath = raw / f"{meta['label']}.json"
        if not jpath.exists():
            runs.append({**meta, "failed": "k6 не записал summary"})
            continue
        s = parse_step(jpath, meta["rate"], meta["duration_s"])
        s.update(meta)
        s["verdict"] = judge(s) or "OK"
        s["resources"] = parse_stats(raw / f"{meta['label']}.stats.tsv")
        dpath = raw / f"{meta['label']}.drain.json"
        if dpath.exists():
            s["drain"] = json.loads(dpath.read_text())
        runs.append(s)

    ladder = sorted((r for r in runs if r.get("kind") == "ladder" and "failed" not in r),
                    key=lambda r: r["rate"])
    knee = None
    for r in ladder:
        if r["verdict"] == "OK":
            knee = r["rate"]
        else:
            break

    run_meta = json.loads((raw / "run-info.json").read_text()) if (raw / "run-info.json").exists() else {}
    integ = json.loads((raw / "integrity.json").read_text()) if (raw / "integrity.json").exists() else None
    if integ is not None:
        acc = sum(r.get("trk_ok_all", 0) for r in runs if "failed" not in r)
        integ["accepted_2xx"] = acc
        integ["rejected_503"] = sum(r.get("trk_503_all", 0) for r in runs if "failed" not in r)
        integ["rejected_429"] = sum(r.get("trk_429_all", 0) for r in runs if "failed" not in r)
        integ["other_errors"] = sum(r.get("trk_other_all", 0) for r in runs if "failed" not in r)
        rows = integ.get("rows_delta")
        if rows is not None and acc:
            integ["loss_pct"] = round((acc - rows) / acc * 100, 4)

    kj = raw / "knee.json"
    knee_info = json.loads(kj.read_text()) if kj.exists() else {}
    result = {"meta": run_meta, "knee_rps": knee, "knee_confirmed_rps": knee_info.get("confirmed_knee"), "runs": runs, "integrity": integ}
    Path(out_json).write_text(json.dumps(result, ensure_ascii=False, indent=2))

    # Markdown-таблица для README / консоли
    print(f"| вид | label | target RPS | achieved | p50 мс | p95 мс | p99 мс | max мс | err | dropped | вердикт |")
    print("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|")
    for r in sorted(runs, key=lambda r: (r.get("kind") != "ladder", r.get("rate", 0), r.get("label", ""))):
        if "failed" in r:
            print(f"| {r.get('kind')} | {r.get('label')} | {r.get('rate')} | - | - | - | - | - | - | - | {r['failed']} |")
            continue
        err = "-" if r["error_rate"] is None else f"{r['error_rate']:.2%}"
        print(f"| {r['kind']} | {r['label']} | {r['rate']} | {r['achieved_rps']:.0f} | {fmt(r['p50_ms'])} | "
              f"{fmt(r['p95_ms'])} | {fmt(r['p99_ms'])} | {fmt(r['max_ms'], 0)} | {err} | "
              f"{r['dropped_iterations']:.0f} | {r['verdict']} |")
    knee_run = next((r for r in ladder if r["rate"] == knee), None)
    if knee_run and knee_run.get("resources"):
        res = knee_run["resources"]
        sut = {n: v for n, v in res.items() if not n.startswith("synbench-k6")}
        tot_mem = sum(v["mem_mib_max"] for v in sut.values())
        tot_cpu = sum(v["cpu_pct_avg"] for v in sut.values())
        print(f"\nРесурсы стека на колене ({knee} RPS, прогрев+замер): суммарный средний CPU {tot_cpu:.0f}% "
              f"(100% = одно ядро; потолок = 100% × число ядер cpuset), сумма пиков памяти {tot_mem:.0f} МиБ")
        print("| контейнер | средний CPU % | пик CPU % | пик памяти МиБ |\n|---|---:|---:|---:|")
        for n, v in sorted(res.items(), key=lambda kv: -kv[1]["cpu_pct_avg"]):
            print(f"| {n} | {v['cpu_pct_avg']:.1f} | {v['cpu_pct_max']:.1f} | {v['mem_mib_max']:.0f} |")
    drains = [(r["label"], r["rate"], r["drain"]) for r in runs if r.get("drain")]
    if drains:
        print("\nДренаж событий после ступени (накопленный backlog принятых, но ещё не записанных событий):")
        print("| label | RPS | backlog при конце k6 | время дренажа, с | дренировано |\n|---|---:|---:|---:|---|")
        for lbl, rate, d in sorted(drains, key=lambda x: (x[1], x[0])):
            print(f"| {lbl} | {rate} | {d['backlog_at_k6_end']} | {d['drain_s']} | {d['drained']} |")
    print(f"\nknee по лестнице (p99<{P99_LIMIT_MS:.0f} мс, ошибки<1%, dropped<1%): {knee} RPS; "
          f"подтверждено повторным прогоном: {knee_info.get('confirmed_knee')}")
    if integ:
        print("целостность:", json.dumps(integ, ensure_ascii=False))


def cmd_judge(step_json, rate, duration_s):
    print(judge(parse_step(step_json, int(rate), float(duration_s))) or "OK")


def cmd_accepted(step_json, rate, duration_s):
    """Сколько событий принял трекер (2xx) за весь запуск k6 (прогрев + замер)."""
    print(int(parse_step(step_json, int(rate), float(duration_s))["trk_ok_all"]))


def cmd_knee(rawdir):
    """Колено лестницы: наибольший ladder-RPS, пройденный без отказов (пусто, если нет)."""
    raw = Path(rawdir)
    knee = ""
    rows = []
    for mp in raw.glob("*.meta.json"):
        meta = json.loads(mp.read_text())
        if meta.get("kind") == "ladder" and (raw / f"{meta['label']}.json").exists():
            rows.append((meta["rate"], meta))
    for rate, meta in sorted(rows, key=lambda x: x[0]):
        s = parse_step(raw / f"{meta['label']}.json", rate, meta["duration_s"])
        if judge(s) is None:
            knee = str(rate)
        else:
            break
    print(knee)


def main(argv):
    if len(argv) >= 5 and argv[1] == "judge":
        cmd_judge(argv[2], argv[3], argv[4])
    elif len(argv) >= 5 and argv[1] == "accepted":
        cmd_accepted(argv[2], argv[3], argv[4])
    elif len(argv) == 3 and argv[1] == "knee":
        cmd_knee(argv[2])
    elif len(argv) >= 5 and argv[1] == "verdict":
        cmd_verdict(argv[2], argv[3], argv[4])
    elif len(argv) == 4 and argv[1] == "aggregate":
        cmd_aggregate(argv[2], argv[3])
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
