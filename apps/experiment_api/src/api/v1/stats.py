"""
Stats API — Единый эндпоинт для умного анализа A/B тестов
===========================================================
GET /api/v1/stats/{experiment_name}

Этот эндпоинт полностью заменяет старый. Он делает всё:
  1. ClickHouse собирает агрегаты (конверсии, ARPU, сегменты, воронку)
  2. Python считает статистику (p-value, CI, Bayesian P(B > A))
  3. Python генерирует бизнес-рекомендации на русском языке

Сложная математика под капотом, бизнес-ответы снаружи.

Параметры запроса:
  - method: frequentist | bayesian | both (default: both)
  - goal_event: целевое событие (default: purchase)
  - segments: сегменты через запятую (default: platform,user_tenure)
  - hide: что скрыть (статистика,бизнес,сегменты,воронка,выбросы,тренд)
  - monthly_traffic: трафик для масштабирования ARPU
"""

from typing import Dict, List, Optional

from fastapi import APIRouter, HTTPException, Query

# --- ClickHouse ---
from src.services.clickhouse_aggregator import (
    fetch_all_metrics,
    fetch_basic_stats,
    fetch_outlier_data,
    fetch_user_event_counts,
    _validate_experiment_name,
)

# --- Анализаторы ---
from src.services.analysis.statistical import StatisticalAnalyzer
from src.services.analysis.business import BusinessAnalyzer
from src.services.analysis.segmentation import (
    SegmentationAnalyzer,
    FunnelAnalyzer,
    OutlierDetector,
    SegmentComparison,
)
from src.services.analysis.recommendations import (
    RecommendationEngine,
    format_segment_insight,
    format_revenue_insight,
    format_guardrail_insight,
)

router = APIRouter(prefix="/stats", tags=["Stats / Analytics"])

# Единый экземпляр каждого анализатора
_stat_analyzer = StatisticalAnalyzer()
_business_analyzer = BusinessAnalyzer()
_seg_analyzer = SegmentationAnalyzer()
_funnel_analyzer = FunnelAnalyzer()
_outlier_detector = OutlierDetector()
_recommender = RecommendationEngine()


@router.get("/{experiment_name}/basic")
async def get_basic_stats_endpoint(experiment_name: str):
    """Базовая статистика по событиям эксперимента (сводка, типы событий, воронка, последние события)."""
    return await fetch_basic_stats(experiment_name)


@router.get("/{experiment_name}")
async def analyze_experiment(
    experiment_name: str,
    method: str = Query("both", regex="^(frequentist|bayesian|both)$"),
    goal_event: str = Query("purchase", min_length=1, max_length=50),
    segments: str = Query("platform,user_tenure", max_length=300),
    hide: str = Query("", max_length=300),
    monthly_traffic: Optional[int] = Query(None, ge=1),
):
    """Полный умный анализ A/B теста.

    Пример ответа:
    {
      "experiment": "button_color_test",
      "recommendations": [
        { "action": "Внедряйте вариант B...", "confidence": "95%", ... }
      ],
      "base_metrics": { ... },
      "statistical": { "p_value": 0.003, "probability_b_better": 0.97, ... },
      "business": { "revenue": { "arpu": {...}, ... }, ... },
      "segmentation": { ... },
      "funnel": { ... },
      "outliers": { ... },
      "daily_trend": [ ... ]
    }
    """
    _validate_experiment_name(experiment_name)

    hidden_sections = set(s.strip().lower() for s in hide.split(",") if s.strip())
    segment_list = [s.strip() for s in segments.split(",") if s.strip()]
    if not segment_list:
        segment_list = ["platform", "user_tenure"]

    # ── 1. Сбор данных из ClickHouse ──
    aggregated = await fetch_all_metrics(
        experiment_name=experiment_name,
        goal_event=goal_event,
        segments=segment_list,
    )

    conversion_rows = aggregated.get("conversion_rates", [])
    if not conversion_rows:
        raise HTTPException(
            status_code=404,
            detail=f"Нет данных по эксперименту '{experiment_name}'",
        )

    # ── 2. Извлекаем variant и control ──
    variant_map = {r["variant"]: r for r in conversion_rows}

    # control = 'A' или 'control'; variant = всё остальное
    control_name = "A" if "A" in variant_map else "control"
    variant_name = next(
        (k for k in variant_map if k != control_name),
        None,
    )

    if not variant_name or control_name not in variant_map:
        raise HTTPException(
            status_code=400,
            detail="Нужны минимум два варианта (control + variant)",
        )

    ctrl = variant_map[control_name]
    var = variant_map[variant_name]

    ctrl_users = int(ctrl.get("total_users", 0))
    ctrl_conversions = int(ctrl.get("converting_users", 0))
    var_users = int(var.get("total_users", 0))
    var_conversions = int(var.get("converting_users", 0))

    if ctrl_users == 0 or var_users == 0:
        raise HTTPException(
            status_code=400,
            detail="В одном из вариантов нет пользователей",
        )

    # ── 3. Статистический анализ ──
    stat_error = None
    stat_result = None

    try:
        stat_result = _stat_analyzer.analyze(
            conversions_variant=var_conversions,
            total_variant=var_users,
            conversions_control=ctrl_conversions,
            total_control=ctrl_users,
            method=method,
        )
    except ValueError as exc:
        stat_error = str(exc)

    # ── 4. Бизнес-анализ ──
    revenue_metrics = None
    revenue_insights = []

    if "бизнес" not in hidden_sections:
        rev_rows = aggregated.get("revenue_metrics") or []
        if not isinstance(rev_rows, list):
            rev_rows = []
        rev_map = {r["variant"]: r for r in rev_rows} if rev_rows else {}

        if variant_name in rev_map and control_name in rev_map:
            rev_var = rev_map[variant_name]
            rev_ctrl = rev_map[control_name]

            revenue_metrics = _business_analyzer.analyze_revenue(
                total_revenue_variant=float(rev_var.get("total_revenue", 0)),
                total_users_variant=int(rev_var.get("total_users", 0)),
                total_revenue_control=float(rev_ctrl.get("total_revenue", 0)),
                total_users_control=int(rev_ctrl.get("total_users", 0)),
                paying_users_variant=int(rev_var.get("paying_users", 0)),
                paying_users_control=int(rev_ctrl.get("paying_users", 0)),
                monthly_traffic=monthly_traffic,
            )

            if revenue_metrics and revenue_metrics.revenue_uplift_relative_pct is not None:
                revenue_insights = format_revenue_insight(
                    arpu_uplift_abs=revenue_metrics.revenue_uplift_absolute or 0.0,
                    arpu_uplift_rel_pct=revenue_metrics.revenue_uplift_relative_pct or 0.0,
                    monthly_impact=revenue_metrics.expected_monthly_revenue_impact,
                )

    # ── 5. Guardrail метрики ──
    guardrail_insights = []
    guardrail_rows = aggregated.get("guardrail_metrics", [])

    if guardrail_rows and not isinstance(guardrail_rows, dict):
        guardrail_results = _business_analyzer.analyze_guardrails(
            _build_guardrail_input(guardrail_rows, variant_name, control_name)
        )
        for _, gr in guardrail_results.items():
            if gr.status != "healthy":
                guardrail_insights.append(
                    format_guardrail_insight(
                        gr.metric_name,
                        gr.relative_change_pct or 0.0,
                        gr.status,
                    )
                )

    # ── 6. Сегментация ──
    segment_analyses = {}
    segment_insights = []

    if "сегменты" not in hidden_sections:
        for seg_field in segment_list:
            seg_rows = aggregated.get(f"segment_{seg_field}", [])
            if not seg_rows or isinstance(seg_rows, dict):
                continue

            comparisons = _build_segment_comparisons(
                seg_rows, variant_name, control_name,
            )
            if comparisons:
                segment_analyses[seg_field] = {
                    "comparisons": [c.to_dict() for c in comparisons],
                }

                strongest = _seg_analyzer.find_strongest_segment_effect(comparisons)
                if strongest:
                    segment_insights.append(
                        format_segment_insight(
                            segment_name=seg_field,
                            segment_value=strongest.segment_value,
                            lift_pct=strongest.relative_lift_pct,
                            is_strongest=True,
                            is_significant=strongest.is_significant,
                        )
                    )

    # ── 7. Воронка ──
    funnel_analysis = None
    if "воронка" not in hidden_sections:
        funnel_rows = aggregated.get("funnel_metrics", [])
        if funnel_rows and not isinstance(funnel_rows, dict):
            funnel_input = _build_funnel_input(funnel_rows, variant_name, control_name)
            if funnel_input:
                funnel_analysis = _funnel_analyzer.analyze_funnel(funnel_input)

    # ── 8. Выбросы ──
    outlier_analysis = None
    if "выбросы" not in hidden_sections:
        try:
            # По доходу
            rev_outliers = await fetch_outlier_data(experiment_name)
            if rev_outliers:
                outlier_vals = [(r["user_id"], float(r["metric_sum"])) for r in rev_outliers]
                outlier_analysis = _outlier_detector.detect_outliers_iqr(outlier_vals, "revenue")

            # По спайкам событий
            event_counts = await fetch_user_event_counts(experiment_name)
            if event_counts:
                spike_vals = [(r["user_id"], int(r["event_count"])) for r in event_counts]
                spike_result = _outlier_detector.detect_event_spike(spike_vals)
                if spike_result.outliers:
                    outlier_analysis = spike_result
        except Exception:
            pass

    # ── 9. Рекомендации (NLP) ──
    recs = _recommender.generate_all(
        stat_result=stat_result or StatisticalAnalyzer().analyze(
            conversions_variant=1, total_variant=2,
            conversions_control=1, total_control=2,
            method=method,
        ),
        variant_name=variant_name,
        control_name=control_name,
        metric_name="конверсия в " + goal_event,
        segment_insights=segment_insights or None,
        revenue_insights=revenue_insights or None,
        guardrail_warnings=guardrail_insights or None,
    )

    # ── 10. Сборка ответа ──
    response = {
        "experiment": experiment_name,
        "goal_event": goal_event,
        "method": method,
        "variants": {
            variant_name: {
                "total_users": var_users,
                "converting_users": var_conversions,
                "conversion_rate": round(var_conversions / var_users, 6) if var_users > 0 else 0,
            },
            control_name: {
                "total_users": ctrl_users,
                "converting_users": ctrl_conversions,
                "conversion_rate": round(ctrl_conversions / ctrl_users, 6) if ctrl_users > 0 else 0,
            },
        },
        "recommendations": [r.to_dict() for r in recs],
        "statistical": stat_result.to_dict() if stat_result else {"error": stat_error},
    }

    if "бизнес" not in hidden_sections:
        response["business"] = {
            "revenue": revenue_metrics.to_dict() if revenue_metrics else None,
            "guardrails": guardrail_insights,
        }

    if "сегменты" not in hidden_sections:
        response["segmentation"] = segment_analyses

    if "воронка" not in hidden_sections:
        response["funnel"] = funnel_analysis.to_dict() if funnel_analysis else None

    if "выбросы" not in hidden_sections:
        response["outliers"] = outlier_analysis.to_dict() if outlier_analysis else None

    if "тренд" not in hidden_sections:
        response["daily_trend"] = aggregated.get("daily_trend", [])

    return response


# ──────────────────────────────────────────────
#  Вспомогательные функции
# ──────────────────────────────────────────────

def _build_guardrail_input(
    rows: List[dict],
    variant_name: str,
    control_name: str,
) -> List[dict]:
    """Превратить строки ClickHouse во вход для BusinessAnalyzer.analyze_guardrails."""
    from collections import defaultdict

    grouped = defaultdict(lambda: {"variant": 0.0, "control": 0.0})
    for row in rows:
        event_name = row["event_name"]
        variant = row["variant"]
        occurrences = int(row.get("total_occurrences", 0))
        users = int(row.get("affected_users", 0))
        rate = occurrences / users if users > 0 else 0.0

        if variant == variant_name:
            grouped[event_name]["variant"] = rate
        else:
            grouped[event_name]["control"] = rate

    warning_labels = {
        "error_occurred": "частота ошибок",
        "support_request": "обращения в поддержку",
        "payment_failed": "отказы платежей",
    }

    result = []
    for event_name, rates in grouped.items():
        result.append({
            "metric_name": warning_labels.get(event_name, event_name),
            "variant_value": rates["variant"],
            "control_value": rates["control"],
            "threshold_direction": "increase",
            "warning_threshold_pct": 10.0,
            "critical_threshold_pct": 25.0,
        })
    return result


def _build_segment_comparisons(
    rows: List[dict],
    variant_name: str,
    control_name: str,
) -> List[SegmentComparison]:
    """Превратить строки сегментации из ClickHouse в список SegmentComparison."""
    from collections import defaultdict

    segments = defaultdict(
        lambda: {"var_users": 0, "ctrl_users": 0, "var_conv": 0, "ctrl_conv": 0}
    )

    for row in rows:
        seg_val = row.get("segment_value", "unknown")
        variant = row["variant"]
        users = int(row.get("total_users", 0))
        conversions = int(row.get("converting_users", 0))

        if variant == variant_name:
            segments[seg_val]["var_users"] = users
            segments[seg_val]["var_conv"] = conversions
        else:
            segments[seg_val]["ctrl_users"] = users
            segments[seg_val]["ctrl_conv"] = conversions

    comparisons = []
    for seg_val, info in segments.items():
        if info["var_users"] == 0 or info["ctrl_users"] == 0:
            continue

        rate_var = info["var_conv"] / info["var_users"]
        rate_ctrl = info["ctrl_conv"] / info["ctrl_users"]
        lift = ((rate_var - rate_ctrl) / rate_ctrl * 100.0) if rate_ctrl > 0 else 0.0

        comp = SegmentComparison(
            segment_name="",
            segment_value=seg_val,
            users_variant=info["var_users"],
            users_control=info["ctrl_users"],
            conversions_variant=info["var_conv"],
            conversions_control=info["ctrl_conv"],
            rate_variant=rate_var,
            rate_control=rate_ctrl,
            relative_lift_pct=lift,
        )

        if info["var_users"] >= 5 and info["ctrl_users"] >= 5:
            p_val = SegmentationAnalyzer._quick_p_value(
                rate_var, rate_ctrl, info["var_users"], info["ctrl_users"],
            )
            comp.p_value = p_val
            comp.is_significant = p_val < 0.05

        comparisons.append(comp)

    return comparisons


def _build_funnel_input(
    rows: List[dict],
    variant_name: str,
    control_name: str,
) -> List[dict]:
    """Превратить строки воронки из ClickHouse во вход для FunnelAnalyzer."""
    from collections import defaultdict

    steps = defaultdict(
        lambda: {"var_reach": 0, "ctrl_reach": 0, "var_total": 0, "ctrl_total": 0, "order": 0}
    )

    for row in rows:
        step_name = row["step_name"]
        step_order = int(row.get("step_order", 0))
        variant = row["variant"]
        reaching = int(row.get("users_reaching", 0))
        total_start = int(row.get("total_users_start", 0))

        steps[step_name]["order"] = step_order
        if variant == variant_name:
            steps[step_name]["var_reach"] = reaching
            steps[step_name]["var_total"] = total_start
        else:
            steps[step_name]["ctrl_reach"] = reaching
            steps[step_name]["ctrl_total"] = total_start

    funnel_input = []
    for step_name, info in sorted(steps.items(), key=lambda x: x[1]["order"]):
        funnel_input.append({
            "step_name": step_name,
            "step_order": info["order"],
            "variant_users_reaching": info["var_reach"],
            "control_users_reaching": info["ctrl_reach"],
            "variant_total_start": info["var_total"],
            "control_total_start": info["ctrl_total"],
        })

    return funnel_input
