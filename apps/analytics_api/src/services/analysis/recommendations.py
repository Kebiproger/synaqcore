"""
Natural Language Recommendation Engine
========================================
Translates raw statistical and business metrics into human-readable,
actionable recommendations.

Key design principles:
1. **Business-first language** — no jargon like "p-value" or "heteroscedasticity"
2. **Action-oriented** — tells the user WHAT to do: "Внедряй вариант B"
3. **Risk-aware** — always mentions confidence level and potential loss
4. **Contextual** — adapts message based on metric type and segment

This is the core differentiator: the service doesn't just show numbers,
it speaks in business language.
"""

from typing import Dict, List, Optional, Tuple

from src.services.analysis.statistical import StatisticalResult


class BusinessRecommendation:
    """A single, actionable recommendation in natural language."""

    def __init__(
        self,
        summary: str,
        action: str,
        confidence: str,
        risk_note: Optional[str] = None,
        additional_insights: Optional[List[str]] = None,
        recommendation_type: str = "overall",
    ):
        """
        Args:
            summary: One-sentence business summary (e.g., "Вариант B увеличивает конверсию на 12%")
            action: Clear call to action (e.g., "Рекомендуется внедрить вариант B")
            confidence: How sure we are (e.g., "Мы уверены на 95%")
            risk_note: What to watch out for (e.g., "Ожидаемые потери при выборе B: 0.3%")
            additional_insights: List of deeper insights
            recommendation_type: 'overall', 'segment', 'guardrail', 'revenue', 'funnel'
        """
        self.summary = summary
        self.action = action
        self.confidence = confidence
        self.risk_note = risk_note
        self.additional_insights = additional_insights or []
        self.recommendation_type = recommendation_type

    def to_dict(self) -> dict:
        return {
            "type": self.recommendation_type,
            "summary": self.summary,
            "action": self.action,
            "confidence": self.confidence,
            "risk_note": self.risk_note,
            "additional_insights": self.additional_insights,
        }


class RecommendationEngine:
    """Generates business-readable recommendations from A/B test data.

    Usage:
        engine = RecommendationEngine()
        recommendations = engine.generate_all(
            stat_result=stat_result,
            variant_name="B",
            control_name="A",
            metric_name="конверсия в покупку",
            ...
        )
    """

    def __init__(self):
        self._locale = "ru"  # Russian language output

    # ─────────────────────────────────────────────
    #  Main entry point
    # ─────────────────────────────────────────────

    def generate_all(
        self,
        stat_result: StatisticalResult,
        variant_name: str = "B",
        control_name: str = "A",
        metric_name: str = "конверсия",
        business_metric_name: str = "",
        segment_insights: Optional[List[str]] = None,
        revenue_insights: Optional[List[str]] = None,
        guardrail_warnings: Optional[List[str]] = None,
    ) -> List[BusinessRecommendation]:
        """Generate all recommendations based on available data.

        Args:
            stat_result: Statistical analysis result
            variant_name: Name of the test variant
            control_name: Name of the control variant
            metric_name: Primary metric name (e.g., "конверсия в покупку")
            business_metric_name: Business name for the metric (e.g., "выручка на пользователя")
            segment_insights: List of segment-specific insight strings
            revenue_insights: List of revenue-specific insight strings
            guardrail_warnings: List of guardrail warning strings

        Returns:
            List of BusinessRecommendation objects, ordered by importance
        """
        recommendations = []

        # 1. Primary recommendation (most important — decision-oriented)
        primary = self._generate_primary_recommendation(
            stat_result, variant_name, control_name, metric_name,
        )
        recommendations.append(primary)

        # 2. Statistical detail recommendation (for users who want more depth)
        stat_detail = self._generate_statistical_detail(
            stat_result, variant_name, control_name,
        )
        recommendations.append(stat_detail)

        # 3. Bayesian interpretation (if available)
        if stat_result.probability_b_better is not None:
            bayes_rec = self._generate_bayesian_recommendation(
                stat_result, variant_name,
            )
            recommendations.append(bayes_rec)

        # 4. Revenue impact (if available)
        if revenue_insights:
            revenue_rec = BusinessRecommendation(
                summary=revenue_insights[0],
                action="Учитывайте влияние на выручку при принятии решения",
                confidence="На основе анализа ARPU",
                risk_note=revenue_insights[1] if len(revenue_insights) > 1 else None,
                additional_insights=revenue_insights[2:] if len(revenue_insights) > 2 else [],
                recommendation_type="revenue",
            )
            recommendations.append(revenue_rec)

        # 5. Segment insights (if available)
        if segment_insights:
            segment_rec = BusinessRecommendation(
                summary="Анализ по сегментам",
                action=segment_insights[0] if segment_insights else "",
                confidence="Основано на данных сегментации",
                additional_insights=segment_insights,
                recommendation_type="segment",
            )
            recommendations.append(segment_rec)

        # 6. Guardrail warnings (if any)
        if guardrail_warnings:
            guard_rec = BusinessRecommendation(
                summary=guardrail_warnings[0],
                action="Проверьте метрики здоровья перед внедрением",
                confidence="Автоматический мониторинг",
                additional_insights=guardrail_warnings,
                recommendation_type="guardrail",
            )
            recommendations.append(guard_rec)

        return recommendations

    # ─────────────────────────────────────────────
    #  Primary recommendation (the "so what?" answer)
    # ─────────────────────────────────────────────

    def _generate_primary_recommendation(
        self,
        stat_result: StatisticalResult,
        variant_name: str,
        control_name: str,
        metric_name: str,
    ) -> BusinessRecommendation:
        """Generate the single most important recommendation.

        This is the answer to the question: "What should I do?"
        """
        is_positive = (
            stat_result.absolute_difference is not None
            and stat_result.absolute_difference > 0
        )

        # Determine if the result is conclusive
        if stat_result.is_significant is True:
            # Statistically significant result
            if is_positive:
                winner = variant_name
                loser = control_name
                action_verb = "Внедряйте"
                direction_desc = "лучше"
            else:
                winner = control_name
                loser = variant_name
                action_verb = "Оставьте"
                direction_desc = "лучше"

            lift_str = self._format_lift(
                stat_result.relative_lift_pct,
                stat_result.absolute_difference,
            )

            summary = (
                f"Вариант **{winner}** {direction_desc} варианта **{loser}** "
                f"по метрике '{metric_name}'. "
                f"Изменение: {lift_str}."
            )

            action = (
                f"{action_verb} вариант **{winner}**. "
                f"Результат статистически значим (p = {stat_result.p_value:.4f})."
            )

            confidence = (
                f"Мы уверены на {stat_result.confidence_level * 100:.0f}%, "
                f"что разница не случайна."
            )

            risk_note = None
            if stat_result.ci_absolute_difference:
                ci = stat_result.ci_absolute_difference
                risk_note = (
                    f"95% доверительный интервал изменения: "
                    f"[{ci[0]:.4f}, {ci[1]:.4f}]. "
                    f"В худшем случае эффект составит {ci[0]:.4f}."
                )

        elif stat_result.is_significant is False and stat_result.p_value is not None:
            # Not significant — need more data
            summary = (
                f"Пока **недостаточно данных** для однозначного вывода "
                f"по метрике '{metric_name}'."
            )
            action = (
                f"Продолжите эксперимент. Текущий p-value = {stat_result.p_value:.4f}, "
                f"что выше порога значимости {stat_result.confidence_level * 100:.0f}%."
            )
            confidence = (
                f"Мы не можем уверенно сказать, какой вариант лучше. "
                f"Разница может быть случайной."
            )
            risk_note = (
                "Ранняя остановка эксперимента может привести к ложноположительному результату. "
            )

            if stat_result.absolute_difference is not None and is_positive:
                risk_note += (
                    f"Текущая тенденция: вариант **{variant_name}** показывает "
                    f"потенциальный рост, но это не подтверждено статистически."
                )

        else:
            # No statistical data available
            summary = (
                f"Для эксперимента '{metric_name}' пока недостаточно событий "
                f"для статистического анализа."
            )
            action = "Соберите больше данных и повторите анализ."
            confidence = "Невозможно оценить."
            risk_note = None

        return BusinessRecommendation(
            summary=summary,
            action=action,
            confidence=confidence,
            risk_note=risk_note,
            recommendation_type="overall",
        )

    # ─────────────────────────────────────────────
    #  Statistical detail (for those who want numbers)
    # ─────────────────────────────────────────────

    def _generate_statistical_detail(
        self,
        stat_result: StatisticalResult,
        variant_name: str,
        control_name: str,
    ) -> BusinessRecommendation:
        """Generate a statistical detail recommendation."""
        insights = []

        if stat_result.rate_variant is not None and stat_result.rate_control is not None:
            insights.append(
                f"Конверсия варианта {variant_name}: {stat_result.rate_variant:.4f} "
                f"({stat_result.rate_variant * 100:.2f}%)"
            )
            insights.append(
                f"Конверсия варианта {control_name}: {stat_result.rate_control:.4f} "
                f"({stat_result.rate_control * 100:.2f}%)"
            )

        if stat_result.ci_variant_rate and stat_result.ci_control_rate:
            insights.append(
                f"95% доверительный интервал для {variant_name}: "
                f"[{stat_result.ci_variant_rate[0]:.4f}, {stat_result.ci_variant_rate[1]:.4f}]"
            )
            insights.append(
                f"95% доверительный интервал для {control_name}: "
                f"[{stat_result.ci_control_rate[0]:.4f}, {stat_result.ci_control_rate[1]:.4f}]"
            )

        if stat_result.relative_lift_pct is not None:
            direction = "выше" if stat_result.relative_lift_pct > 0 else "ниже"
            insights.append(
                f"Относительное изменение: {abs(stat_result.relative_lift_pct):.2f}% "
                f"({direction})"
            )

        return BusinessRecommendation(
            summary="Детальная статистика эксперимента",
            action="Сравните метрики для принятия решения",
            confidence=f"Уровень значимости: {1 - (stat_result.p_value or 0.05):.0%}",
            additional_insights=insights,
            recommendation_type="statistical_detail",
        )

    # ─────────────────────────────────────────────
    #  Bayesian recommendation
    # ─────────────────────────────────────────────

    def _generate_bayesian_recommendation(
        self,
        stat_result: StatisticalResult,
        variant_name: str,
    ) -> BusinessRecommendation:
        """Generate a Bayesian-focused recommendation.

        Bayesian metrics are more intuitive for business stakeholders
        because they answer the direct question: "What's the probability
        that we're making the right choice?"
        """
        prob = stat_result.probability_b_better or 0.5
        prob_pct = prob * 100.0

        if prob > 0.95:
            confidence_desc = "очень высокая"
            action = f"С высокой вероятностью вариант **{variant_name}** — правильный выбор."
        elif prob > 0.80:
            confidence_desc = "высокая"
            action = f"Вероятно, вариант **{variant_name}** лучше. Риск ошибки невелик."
        elif prob > 0.50:
            confidence_desc = "умеренная"
            action = (
                f"Вариант **{variant_name}** имеет преимущество, "
                f"но рекомендуется продолжить сбор данных."
            )
        else:
            confidence_desc = "низкая"
            action = "Недостаточно данных для уверенного выбора."

        summary = (
            f"Байесовский анализ: вероятность того, что вариант **{variant_name}** лучше "
            f"— {prob_pct:.1f}%. Это {confidence_desc} уверенность."
        )

        risk_note = None
        if stat_result.expected_loss_choose_b is not None:
            risk_note = (
                f"Ожидаемые потери при выборе {variant_name}: "
                f"{stat_result.expected_loss_choose_b:.4f} "
                f"(в единицах метрики). "
                f"Это приемлемый уровень риска."
                if stat_result.expected_loss_choose_b < 0.01
                else (
                    f"Ожидаемые потери при выборе {variant_name}: "
                    f"{stat_result.expected_loss_choose_b:.4f}. "
                    f"Учитывайте этот риск при принятии решения."
                )
            )

        return BusinessRecommendation(
            summary=summary,
            action=action,
            confidence=f"{confidence_desc} ({prob_pct:.1f}%)",
            risk_note=risk_note,
            recommendation_type="bayesian",
        )

    # ─────────────────────────────────────────────
    #  Helper: format lift for display
    # ─────────────────────────────────────────────

    @staticmethod
    def _format_lift(
        relative_lift_pct: Optional[float],
        absolute_diff: Optional[float],
    ) -> str:
        """Format the lift value in a human-readable way."""
        if relative_lift_pct is None and absolute_diff is None:
            return "нет данных"

        parts = []
        if relative_lift_pct is not None:
            direction = "+" if relative_lift_pct >= 0 else ""
            parts.append(f"{direction}{relative_lift_pct:.2f}% относительного изменения")

        if absolute_diff is not None:
            direction = "+" if absolute_diff >= 0 else ""
            parts.append(f"{direction}{absolute_diff:.4f} абсолютного изменения")

        return " (" + ", ".join(parts) + ")" if parts else "нет данных"


# ─────────────────────────────────────────────
#  Convenience: quick insight generators
# ─────────────────────────────────────────────

def format_segment_insight(
    segment_name: str,
    segment_value: str,
    lift_pct: float,
    is_strongest: bool = False,
    is_significant: bool = False,
) -> str:
    """Format a single segment insight."""
    prefix = "🔝 Сильнейший эффект" if is_strongest else f"Сегмент '{segment_name}: {segment_value}'"
    significance = "✅ статистически значимо" if is_significant else "⚠️ требуется больше данных"

    return (
        f"{prefix}: {lift_pct:+.1f}% к конверсии ({significance})"
    )


def format_revenue_insight(
    arpu_uplift_abs: float,
    arpu_uplift_rel_pct: float,
    monthly_impact: Optional[float] = None,
) -> List[str]:
    """Format revenue insights."""
    insights = []

    direction = "выше" if arpu_uplift_abs >= 0 else "ниже"
    insights.append(
        f"Выручка на пользователя (ARPU) {direction} на "
        f"{abs(arpu_uplift_rel_pct):.1f}% ({arpu_uplift_abs:+.4f} у.е.)"
    )

    if monthly_impact is not None:
        sign = "+" if monthly_impact >= 0 else ""
        insights.append(
            f"Ожидаемый месячный эффект: {sign}{monthly_impact:.2f} у.е. "
            f"при текущем трафике"
        )

    return insights


def format_guardrail_insight(
    metric_name: str,
    change_pct: float,
    status: str,
) -> str:
    """Format a guardrail metric insight."""
    if status == "healthy":
        return f"✅ Метрика '{metric_name}' в норме (изменение: {change_pct:+.1f}%)"
    elif status == "warning":
        return f"⚠️ Метрика '{metric_name}' вызывает беспокойство ({change_pct:+.1f}%)"
    else:
        return f"🚫 Критическое изменение метрики '{metric_name}' ({change_pct:+.1f}%)"