"""
Тесты для анализа A/B тестов
=============================
Проверяют:
  1. Статистический анализ (p-value, CI, Bayesian)
  2. Бизнес-метрики (ARPU, revenue uplift)
  3. Сегментацию и воронку
  4. Обнаружение выбросов
  5. Генерацию рекомендаций
"""

import pytest

from tests._isolated_import import ANALYTICS_API_DIR, load_analytics_modules

# Движок анализа живёт только в analytics_api: загружаем его изолированно,
# т.к. conftest.py кладёт в sys.path experiment_api (оба сервиса — пакет `src`).
_statistical, _business, _segmentation, _recommendations = load_analytics_modules(
    "src.services.analysis.statistical",
    "src.services.analysis.business",
    "src.services.analysis.segmentation",
    "src.services.analysis.recommendations",
)

StatisticalAnalyzer = _statistical.StatisticalAnalyzer
StatisticalResult = _statistical.StatisticalResult
_safe_rate = _statistical._safe_rate
_safe_standard_error = _statistical._safe_standard_error
BusinessAnalyzer = _business.BusinessAnalyzer
RevenueMetrics = _business.RevenueMetrics
GuardrailMetric = _business.GuardrailMetric
SegmentationAnalyzer = _segmentation.SegmentationAnalyzer
FunnelAnalyzer = _segmentation.FunnelAnalyzer
OutlierDetector = _segmentation.OutlierDetector
SegmentComparison = _segmentation.SegmentComparison
RecommendationEngine = _recommendations.RecommendationEngine
BusinessRecommendation = _recommendations.BusinessRecommendation


def test_engine_is_from_analytics_api():
    for module in (_statistical, _business, _segmentation, _recommendations):
        assert module.__file__.startswith(ANALYTICS_API_DIR)


# ──────────────────────────────────────────────
#  Tests: Statistical Analyzer
# ──────────────────────────────────────────────

class TestStatisticalAnalyzer:
    """Тесты для статистического анализатора."""

    @pytest.fixture
    def analyzer(self):
        return StatisticalAnalyzer()

    def test_basic_conversion_analysis(self, analyzer):
        """Базовый тест: конверсия A vs B.

        12% vs 10% с n=1000 НЕ статистически значимо (p ≈ 0.15).
        Это реалистичный тест — разница небольшая.
        """
        result = analyzer.analyze(
            conversions_variant=120, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="both",
        )

        # Проверка базовых метрик
        assert result.rate_variant == pytest.approx(0.12, rel=1e-6)
        assert result.rate_control == pytest.approx(0.10, rel=1e-6)
        assert result.absolute_difference == pytest.approx(0.02, rel=1e-6)
        assert result.relative_lift_pct == pytest.approx(20.0, rel=0.1)

        # Проверка: p-value > 0.05 (НЕ значимо для n=1000)
        # Это реалистично — разница 2% при n=1000 не значима
        assert result.p_value > 0.1  # p ≈ 0.15
        assert result.is_significant is False

        # Bayesian вероятность должна быть выше 50%, но не экстремальной
        assert result.probability_b_better > 0.5
        assert result.probability_b_better < 0.95

    def test_significant_difference(self, analyzer):
        """Тест: статистически значимая разница.

        15% vs 10% с n=1000 — это значимо (p < 0.01).
        """
        result = analyzer.analyze(
            conversions_variant=150, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="both",
        )

        assert result.is_significant is True
        assert result.p_value < 0.01

        # Bayesian вероятность должна быть высокой
        assert result.probability_b_better > 0.95

    def test_no_significant_difference(self, analyzer):
        """Тест: нет статистически значимой разницы."""
        result = analyzer.analyze(
            conversions_variant=105, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="both",
        )

        # Разница незначима
        assert result.is_significant is False
        assert result.p_value > 0.05

        # Bayesian вероятность близка к 50%
        assert result.probability_b_better is not None
        assert 0.5 < result.probability_b_better < 0.7

    def test_negative_effect(self, analyzer):
        """Тест: негативный эффект (вариант хуже)."""
        result = analyzer.analyze(
            conversions_variant=80, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="both",
        )

        assert result.absolute_difference < 0
        assert result.relative_lift_pct < 0
        # Вероятность что B лучше — низкая
        assert result.probability_b_better < 0.1

    def test_wilson_confidence_interval(self, analyzer):
        """Тест: доверительный интервал Вильсона."""
        result = analyzer.analyze(
            conversions_variant=50, total_variant=1000,
            conversions_control=50, total_control=1000,
            method="frequentist",
        )

        # CI должен быть валидным (нижний < верхний)
        assert result.ci_variant_rate[0] < result.ci_variant_rate[1]
        assert 0 <= result.ci_variant_rate[0] <= 1
        assert 0 <= result.ci_variant_rate[1] <= 1

    def test_zero_conversion(self, analyzer):
        """Тест: нулевая конверсия (edge case)."""
        result = analyzer.analyze(
            conversions_variant=0, total_variant=1000,
            conversions_control=50, total_control=1000,
            method="both",
        )

        assert result.rate_variant == 0.0
        assert result.rate_control == pytest.approx(0.05, rel=1e-6)
        assert result.is_significant is True

    def test_invalid_inputs(self, analyzer):
        """Тест: невалидные входные данные."""
        with pytest.raises(ValueError):
            # Ноль пользователей
            analyzer.analyze(
                conversions_variant=10, total_variant=0,
                conversions_control=10, total_control=100,
            )

        with pytest.raises(ValueError):
            # Отрицательные конверсии
            analyzer.analyze(
                conversions_variant=-5, total_variant=100,
                conversions_control=10, total_control=100,
            )

        with pytest.raises(ValueError):
            # Конверсии > всего
            analyzer.analyze(
                conversions_variant=150, total_variant=100,
                conversions_control=10, total_control=100,
            )

    def test_bayesian_expected_loss(self, analyzer):
        """Тест: Expected Loss (риск ошибки)."""
        result = analyzer.analyze(
            conversions_variant=150, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="bayesian",
        )

        # Expected Loss должен быть положительным и небольшим
        assert result.expected_loss_choose_b is not None
        assert result.expected_loss_choose_b > 0
        assert result.expected_loss_choose_b < 0.05  # < 5% риска

    def test_credible_intervals(self, analyzer):
        """Тест: байесовские доверительные интервалы."""
        result = analyzer.analyze(
            conversions_variant=150, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="bayesian",
        )

        # CI должны быть валидными
        assert result.bayesian_credible_interval_b[0] < result.bayesian_credible_interval_b[1]
        assert 0 <= result.bayesian_credible_interval_b[0] <= 1
        assert 0 <= result.bayesian_credible_interval_b[1] <= 1

    def test_large_sample_significance(self, analyzer):
        """Тест: большой выбор — даже маленькая разница значима."""
        # 11% vs 10% с n=10000 — это значимо
        result = analyzer.analyze(
            conversions_variant=1100, total_variant=10000,
            conversions_control=1000, total_control=10000,
            method="both",
        )

        assert result.is_significant is True
        assert result.p_value < 0.05
        assert result.probability_b_better > 0.95


# ──────────────────────────────────────────────
#  Tests: Business Analyzer
# ──────────────────────────────────────────────

class TestBusinessAnalyzer:
    """Тесты для бизнес-анализатора."""

    @pytest.fixture
    def analyzer(self):
        return BusinessAnalyzer()

    def test_arpu_analysis(self, analyzer):
        """Тест: ARPU (средняя выручка на пользователя)."""
        metrics = analyzer.analyze_revenue(
            total_revenue_variant=12000.0, total_users_variant=1000,
            total_revenue_control=10000.0, total_users_control=1000,
            paying_users_variant=150, paying_users_control=120,
            monthly_traffic=10000,
        )

        # ARPU
        assert metrics.arpu_variant == pytest.approx(12.0, rel=1e-6)
        assert metrics.arpu_control == pytest.approx(10.0, rel=1e-6)
        assert metrics.revenue_uplift_absolute == pytest.approx(2.0, rel=1e-6)
        assert metrics.revenue_uplift_relative_pct == pytest.approx(20.0, rel=0.1)

        # Ожидаемый месячный эффект
        assert metrics.expected_monthly_revenue_impact == pytest.approx(20000.0, rel=1e-6)

    def test_arppu_analysis(self, analyzer):
        """Тест: ARPPU (средняя выручка на платящего пользователя)."""
        metrics = analyzer.analyze_revenue(
            total_revenue_variant=12000.0, total_users_variant=1000,
            total_revenue_control=10000.0, total_users_control=1000,
            paying_users_variant=150, paying_users_control=120,
        )

        assert metrics.arppu_variant == pytest.approx(80.0, rel=1e-6)
        assert metrics.arppu_control == pytest.approx(83.33, rel=0.01)

        # Paying user rate
        assert metrics.paying_user_rate_variant == pytest.approx(0.15, rel=1e-6)
        assert metrics.paying_user_rate_control == pytest.approx(0.12, rel=1e-6)

    def test_guardrail_metric_healthy(self, analyzer):
        """Тест: guardrail метрика в норме."""
        guardrail = GuardrailMetric(
            metric_name="Error Rate",
            variant_value=0.01,
            control_value=0.01,
            threshold_direction="increase",
        )

        assert guardrail.status == "healthy"
        assert "stable" in guardrail.message

    def test_guardrail_metric_warning(self, analyzer):
        """Тест: guardrail метрика в предупреждении."""
        # 50% увеличение — это warning, не critical
        guardrail = GuardrailMetric(
            metric_name="Error Rate",
            variant_value=0.015,
            control_value=0.01,
            threshold_direction="increase",
            warning_threshold_pct=5.0,
            critical_threshold_pct=50.0,  # Увеличили порог
        )

        assert guardrail.status == "warning"
        assert "WARNING" in guardrail.message

    def test_guardrail_metric_critical(self, analyzer):
        """Тест: guardrail метрика критична."""
        guardrail = GuardrailMetric(
            metric_name="Error Rate",
            variant_value=0.025,
            control_value=0.01,
            threshold_direction="increase",
            warning_threshold_pct=5.0,
            critical_threshold_pct=15.0,
        )

        assert guardrail.status == "critical"
        assert "CRITICAL" in guardrail.message

    def test_format_uplift_summary(self, analyzer):
        """Тест: форматирование аплифта для бизнеса."""
        summary = analyzer.format_uplift_summary(
            absolute_difference=0.02,
            relative_lift_pct=20.0,
            metric_name="конверсия в покупку",
            is_statistically_significant=True,
            is_positive=True,
        )

        assert "Вариант B" in summary
        assert "20.0%" in summary
        assert "Статистически значимо" in summary


# ──────────────────────────────────────────────
#  Tests: Segmentation Analyzer
# ──────────────────────────────────────────────

class TestSegmentationAnalyzer:
    """Тесты для анализатора сегментов."""

    @pytest.fixture
    def analyzer(self):
        return SegmentationAnalyzer()

    def test_segment_comparison(self, analyzer):
        """Тест: сравнение сегментов."""
        segment_data = [
            {"segment_value": "iOS", "variant_users": 500, "control_users": 500,
             "variant_conversions": 60, "control_conversions": 50},
            {"segment_value": "Android", "variant_users": 500, "control_users": 500,
             "variant_conversions": 60, "control_conversions": 50},
        ]

        comparisons = analyzer.analyze_segment(
            segment_name="platform",
            segment_data=segment_data,
        )

        assert len(comparisons) == 2

        # Проверка iOS
        ios = next(c for c in comparisons if c.segment_value == "iOS")
        assert ios.rate_variant == pytest.approx(0.12, rel=1e-6)
        assert ios.rate_control == pytest.approx(0.10, rel=1e-6)
        assert ios.relative_lift_pct == pytest.approx(20.0, rel=0.1)

    def test_simpsons_paradox_detection(self, analyzer):
        """Тест: обнаружение парадокса Симпсона.

        Парадокс Симпсона: общий эффект положительный,
        но в большинстве сегментов эффект отрицательный.
        """
        segment_data = [
            {"segment_value": "iOS", "variant_users": 500, "control_users": 500,
             "variant_conversions": 40, "control_conversions": 50},  # iOS: B хуже (-20%)
            {"segment_value": "Android", "variant_users": 500, "control_users": 500,
             "variant_conversions": 80, "control_conversions": 50},  # Android: B лучше (+60%)
        ]

        comparisons = analyzer.analyze_segment(
            segment_name="platform",
            segment_data=segment_data,
        )

        # Проверим, что сравнения рассчитаны
        assert len(comparisons) == 2
        
        # Общий uplift положительный (60+40)/(500+500) - 50/1000 = 0.12 - 0.05 = 0.07
        # Но в одном сегменте (iOS) эффект отрицательный
        # Парадокс Симпсона НЕ обнаружится, так как только 1 из 2 сегментов в обратную сторону
        # (нужно > 50% сегментов)
        paradox_detected, detail = analyzer.check_simpsons_paradox(
            comparisons,
            overall_lift_pct=20.0,  # Общий uplift положительный
            segment_name="platform",
        )
        
        # Не обнаружим парадокс — только 1 из 2 сегментов в обратную сторону
        assert paradox_detected is False

    def test_simpsons_paradox_strong(self, analyzer):
        """Тест: сильный парадокс Симпсона (3 из 4 сегментов в обратную сторону)."""
        segment_data = [
            {"segment_value": "A", "variant_users": 500, "control_users": 500,
             "variant_conversions": 40, "control_conversions": 50},  # -20%
            {"segment_value": "B", "variant_users": 500, "control_users": 500,
             "variant_conversions": 40, "control_conversions": 50},  # -20%
            {"segment_value": "C", "variant_users": 500, "control_users": 500,
             "variant_conversions": 40, "control_conversions": 50},  # -20%
            {"segment_value": "D", "variant_users": 500, "control_users": 500,
             "variant_conversions": 200, "control_conversions": 50},  # +300% (сильный outlier)
        ]

        comparisons = analyzer.analyze_segment(
            segment_name="platform",
            segment_data=segment_data,
        )

        # Общий uplift положительный (320/2000 - 200/2000 = 0.06)
        paradox_detected, detail = analyzer.check_simpsons_paradox(
            comparisons,
            overall_lift_pct=20.0,
            segment_name="platform",
        )

        # Должен обнаружить парадокс (3 из 4 сегментов в обратную сторону)
        assert paradox_detected is True

    def test_find_strongest_segment(self, analyzer):
        """Тест: поиск сегмента с самым сильным эффектом."""
        segment_data = [
            {"segment_value": "iOS", "variant_users": 500, "control_users": 500,
             "variant_conversions": 60, "control_conversions": 50},  # +20%
            {"segment_value": "Android", "variant_users": 500, "control_users": 500,
             "variant_conversions": 90, "control_conversions": 50},  # +80%
        ]

        comparisons = analyzer.analyze_segment(
            segment_name="platform",
            segment_data=segment_data,
        )

        strongest = analyzer.find_strongest_segment_effect(comparisons)

        assert strongest.segment_value == "Android"
        assert strongest.relative_lift_pct == pytest.approx(80.0, rel=0.1)


# ──────────────────────────────────────────────
#  Tests: Funnel Analyzer
# ──────────────────────────────────────────────

class TestFunnelAnalyzer:
    """Тесты для анализатора воронки."""

    @pytest.fixture
    def analyzer(self):
        return FunnelAnalyzer()

    def test_simple_funnel(self, analyzer):
        """Тест: простая воронка из 3 шагов."""
        funnel_data = [
            {"step_name": "page_view", "step_order": 1,
             "variant_users_reaching": 1000, "control_users_reaching": 1000,
             "variant_total_start": 1000, "control_total_start": 1000},
            {"step_name": "add_to_cart", "step_order": 2,
             "variant_users_reaching": 300, "control_users_reaching": 250,
             "variant_total_start": 1000, "control_total_start": 1000},
            {"step_name": "purchase", "step_order": 3,
             "variant_users_reaching": 120, "control_users_reaching": 100,
             "variant_total_start": 1000, "control_total_start": 1000},
        ]

        result = analyzer.analyze_funnel(funnel_data)

        assert len(result.funnel_steps) == 3
        assert result.variant_overall_conversion == pytest.approx(0.12, rel=1e-6)
        assert result.control_overall_conversion == pytest.approx(0.10, rel=1e-6)

    def test_funnel_bottleneck_detection(self, analyzer):
        """Тест: обнаружение узкого места в воронке."""
        funnel_data = [
            {"step_name": "page_view", "step_order": 1,
             "variant_users_reaching": 1000, "control_users_reaching": 1000,
             "variant_total_start": 1000, "control_total_start": 1000},
            {"step_name": "add_to_cart", "step_order": 2,
             "variant_users_reaching": 300, "control_users_reaching": 250,
             "variant_total_start": 1000, "control_total_start": 1000},
            {"step_name": "checkout_start", "step_order": 3,
             "variant_users_reaching": 150, "control_users_reaching": 140,
             "variant_total_start": 1000, "control_total_start": 1000},
            {"step_name": "purchase", "step_order": 4,
             "variant_users_reaching": 120, "control_users_reaching": 100,
             "variant_total_start": 1000, "control_total_start": 1000},
        ]

        result = analyzer.analyze_funnel(funnel_data)

        # bottleneck должен быть самым большим drop-off
        assert result.bottleneck_step == "add_to_cart"


# ──────────────────────────────────────────────
#  Tests: Outlier Detector
# ──────────────────────────────────────────────

class TestOutlierDetector:
    """Тесты для детектора выбросов."""

    @pytest.fixture
    def detector(self):
        return OutlierDetector()

    def test_detect_revenue_outliers(self, detector):
        """Тест: обнаружение выбросов по выручке."""
        values = [
            ("user1", 10.0), ("user2", 15.0), ("user3", 12.0),
            ("user4", 11.0), ("user5", 14.0),
            ("user6", 1000.0),  # явный выброс
        ]

        result = detector.detect_outliers_iqr(values, metric_name="revenue")

        assert result.total_outliers_removed >= 1
        assert len(result.outliers) >= 1
        assert result.outliers[0].user_id == "user6"

    def test_detect_event_spikes(self, detector):
        """Тест: обнаружение спайков событий (боты)."""
        values = [
            ("user1", 10), ("user2", 12), ("user3", 11),
            ("user4", 1000),  # явный спайк
            ("user5", 9),
        ]

        result = detector.detect_event_spike(values)

        assert result.total_outliers_removed >= 1
        assert result.outliers[0].user_id == "user4"

    def test_no_outliers(self, detector):
        """Тест: нет выбросов в нормальных данных."""
        values = [
            ("user1", 10.0), ("user2", 12.0), ("user3", 11.0),
            ("user4", 13.0), ("user5", 10.0),
        ]

        result = detector.detect_outliers_iqr(values, metric_name="revenue")

        assert result.total_outliers_removed == 0
        assert len(result.outliers) == 0


# ──────────────────────────────────────────────
#  Tests: Recommendation Engine
# ──────────────────────────────────────────────

class TestRecommendationEngine:
    """Тесты для генератора рекомендаций."""

    @pytest.fixture
    def engine(self):
        return RecommendationEngine()

    def test_generate_positive_recommendation(self, engine):
        """Тест: генерация рекомендации при положительном эффекте."""
        stat_result = StatisticalAnalyzer().analyze(
            conversions_variant=150, total_variant=1000,  # 15% vs 10% — значимо
            conversions_control=100, total_control=1000,
            method="both",
        )

        recs = engine.generate_all(
            stat_result=stat_result,
            variant_name="B",
            control_name="A",
            metric_name="конверсия в покупку",
        )

        assert len(recs) >= 1
        # Должна быть рекомендация внедрить B
        assert any("Внедряйте" in r.action or "Вариант B" in r.summary for r in recs)

    def test_generate_no_significant_recommendation(self, engine):
        """Тест: рекомендация при незначимом эффекте."""
        stat_result = StatisticalAnalyzer().analyze(
            conversions_variant=105, total_variant=1000,
            conversions_control=100, total_control=1000,
            method="both",
        )

        recs = engine.generate_all(
            stat_result=stat_result,
            variant_name="B",
            control_name="A",
            metric_name="конверсия в покупку",
        )

        assert any("недостаточно данных" in r.summary.lower() or
                   "продолжите" in r.action.lower()
                   for r in recs)

    def test_generate_negative_recommendation(self, engine):
        """Тест: рекомендация при негативном эффекте."""
        stat_result = StatisticalAnalyzer().analyze(
            conversions_variant=70, total_variant=1000,  # 7% vs 10% — значимо
            conversions_control=100, total_control=1000,
            method="both",
        )

        recs = engine.generate_all(
            stat_result=stat_result,
            variant_name="B",
            control_name="A",
            metric_name="конверсия в покупку",
        )

        # Должна быть рекомендация оставить A
        assert any("Оставьте" in r.action or "вариант A" in r.summary
                   for r in recs)


# ──────────────────────────────────────────────
#  Tests: Helper Functions
# ──────────────────────────────────────────────

class TestHelperFunctions:
    """Тесты для вспомогательных функций."""

    def test_safe_rate(self):
        """Тест: безопасный расчет rate."""
        assert _safe_rate(10, 100) == pytest.approx(0.1, rel=1e-6)
        assert _safe_rate(10, 0) == 0.0
        assert _safe_rate(0, 100) == 0.0

    def test_safe_standard_error(self):
        """Тест: безопасный расчет SE."""
        assert _safe_standard_error(0.1, 100) == pytest.approx(0.03, rel=0.01)
        assert _safe_standard_error(0.1, 0) == 0.0


# ──────────────────────────────────────────────
#  Run Tests
# ──────────────────────────────────────────────

if __name__ == "__main__":
    pytest.main([__file__, "-v"])
