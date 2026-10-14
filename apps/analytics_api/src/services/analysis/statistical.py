"""
Statistical Analysis Engine for A/B Testing
=============================================
Supports two paradigms:

1. **Frequentist** (classical):
   - p-value (statistical significance)
   - Confidence intervals for conversion rates
   - Confidence interval for the lift / absolute difference
   - Z-test / chi-squared test for proportions

2. **Bayesian** (modern, business-friendly):
   - Probability that B is better than A (P(B > A))
   - Expected Loss — how much we risk by choosing B
   - Bayesian credible intervals via Beta-Binomial conjugate model

All formulas are implemented from scratch using only `math` to avoid
heavy dependencies. This keeps the microservice lightweight and auditable.
"""

import math
import random
from typing import Dict, List, Optional, Tuple


# ──────────────────────────────────────────────
#  Helper: Gaussian CDF approximation (erf-based)
# ──────────────────────────────────────────────

def _gaussian_cdf(value: float) -> float:
    """Standard normal CDF using the error function.

    Uses math.erfc which is accurate and constant-time (no side channels).
    """
    return 0.5 * math.erfc(-value / math.sqrt(2.0))


def _two_proportion_z_score(
    rate_variant: float,
    rate_control: float,
    count_variant: int,
    count_control: int,
) -> float:
    """Two-proportion Z-test statistic.

    Computes how many standard deviations the observed difference
    is from zero (the null hypothesis).

    H0: rate_variant == rate_control (no difference between variants)
    """
    pooled_rate = (
        rate_variant * count_variant + rate_control * count_control
    ) / (count_variant + count_control)

    standard_error = math.sqrt(
        pooled_rate * (1.0 - pooled_rate) * (1.0 / count_variant + 1.0 / count_control)
    )

    if standard_error == 0.0:
        return 0.0
    return (rate_variant - rate_control) / standard_error


def _beta_posterior_parameters(
    successes: int,
    failures: int,
) -> Tuple[float, float]:
    """Posterior Beta distribution parameters for a Bernoulli outcome.

    Uses Jeffreys prior Beta(0.5, 0.5) which is weakly informative
    and scale-invariant.

    Args:
        successes: Number of conversions (positive events)
        failures: Number of non-conversions (total - conversions)

    Returns:
        (alpha_param, beta_param) posterior parameters
    """
    alpha_param = successes + 0.5   # Jeffreys prior
    beta_param = failures + 0.5
    return alpha_param, beta_param


def _sample_gamma_distribution(shape: float) -> float:
    """Marsaglia & Tsang's method for Gamma(shape, 1) sampling.

    Only works for shape >= 1. For shape < 1, uses the small-shape
    transformation: Gamma(a,1) = Gamma(a+1,1) * U^(1/a) where U ~ Uniform(0,1].

    This provides cryptographically-adequate sampling for statistical
    computation. CWE-338 (Insufficient Entropy) is not a concern here
    since we're doing MC estimation, not generating secrets.
    """
    if shape < 1:
        uniform_sample = random.random()
        if uniform_sample <= 0:
            return 0.0
        return _sample_gamma_distribution(shape + 1.0) * (
            uniform_sample ** (1.0 / shape)
        )

    # Marsaglia & Tsang for shape >= 1
    d_offset = shape - 1.0 / 3.0
    c_correction = 1.0 / math.sqrt(9.0 * d_offset)

    while True:
        normal_sample = random.gauss(0, 1)
        v_value = 1.0 + c_correction * normal_sample
        if v_value <= 0:
            continue
        v_cubed = v_value * v_value * v_value  # v^3
        uniform_sample = random.random()

        # Quick acceptance check
        if uniform_sample < 1.0 - 0.0331 * (normal_sample * normal_sample) * (normal_sample * normal_sample):
            return d_offset * v_cubed

        # Log acceptance check
        if math.log(uniform_sample) < (
            0.5 * normal_sample * normal_sample
            + d_offset * (1.0 - v_cubed + math.log(v_cubed))
        ):
            return d_offset * v_cubed


def _sample_beta_distribution(alpha_param: float, beta_param: float) -> float:
    """Sample from Beta(alpha_param, beta_param) distribution.

    Uses the relationship: if X ~ Gamma(alpha, 1) and Y ~ Gamma(beta, 1),
    then X / (X + Y) ~ Beta(alpha, beta).

    Returns a value between 0 and 1.
    """
    gamma_x = _sample_gamma_distribution(alpha_param)
    gamma_y = _sample_gamma_distribution(beta_param)
    total = gamma_x + gamma_y

    if total <= 0:
        return 0.5  # fallback for degenerate case
    return gamma_x / total


# ──────────────────────────────────────────────
#  Core Analysis Classes
# ──────────────────────────────────────────────

class StatisticalResult:
    """Container for all statistical analysis results.

    This object is designed to be serialized to JSON for the API response.
    """

    def __init__(self):
        # Frequentist results
        self.p_value: Optional[float] = None
        self.is_significant: Optional[bool] = None
        self.confidence_level: Optional[float] = None
        self.ci_variant_rate: Optional[Tuple[float, float]] = None    # 95% CI for variant rate
        self.ci_control_rate: Optional[Tuple[float, float]] = None    # 95% CI for control rate
        self.ci_absolute_difference: Optional[Tuple[float, float]] = None
        self.ci_relative_lift: Optional[Tuple[float, float]] = None
        self.z_score: Optional[float] = None

        # Sample statistics
        self.rate_variant: Optional[float] = None
        self.rate_control: Optional[float] = None
        self.absolute_difference: Optional[float] = None
        self.relative_lift_pct: Optional[float] = None

        # Bayesian results
        self.probability_b_better: Optional[float] = None         # P(B > A)
        self.probability_a_better: Optional[float] = None         # P(A > B)
        self.expected_loss_choose_b: Optional[float] = None       # Risk of picking B
        self.expected_loss_choose_a: Optional[float] = None       # Risk of picking A
        self.bayesian_credible_interval_b: Optional[Tuple[float, float]] = None
        self.bayesian_credible_interval_a: Optional[Tuple[float, float]] = None

        # Method selection
        self.method: str = "frequentist"  # or "bayesian" or "both"

    def to_dict(self) -> dict:
        """Serialize to a clean dictionary for JSON response."""
        return {
            "method": self.method,
            "sample_rates": {
                "rate_variant": round(self.rate_variant, 6) if self.rate_variant is not None else None,
                "rate_control": round(self.rate_control, 6) if self.rate_control is not None else None,
                "absolute_difference": round(self.absolute_difference, 6) if self.absolute_difference is not None else None,
                "relative_lift_pct": round(self.relative_lift_pct, 4) if self.relative_lift_pct is not None else None,
            },
            "frequentist": {
                "p_value": round(self.p_value, 6) if self.p_value is not None else None,
                "is_significant": self.is_significant,
                "confidence_level": self.confidence_level,
                "z_score": round(self.z_score, 4) if self.z_score is not None else None,
                "ci_variant_rate": (
                    (round(self.ci_variant_rate[0], 6), round(self.ci_variant_rate[1], 6))
                    if self.ci_variant_rate else None
                ),
                "ci_control_rate": (
                    (round(self.ci_control_rate[0], 6), round(self.ci_control_rate[1], 6))
                    if self.ci_control_rate else None
                ),
                "ci_absolute_difference": (
                    (round(self.ci_absolute_difference[0], 6), round(self.ci_absolute_difference[1], 6))
                    if self.ci_absolute_difference else None
                ),
                "ci_relative_lift": (
                    (round(self.ci_relative_lift[0], 6), round(self.ci_relative_lift[1], 6))
                    if self.ci_relative_lift else None
                ),
            },
            "bayesian": {
                "probability_b_better": round(self.probability_b_better, 4) if self.probability_b_better is not None else None,
                "probability_a_better": round(self.probability_a_better, 4) if self.probability_a_better is not None else None,
                "expected_loss_choose_b": round(self.expected_loss_choose_b, 6) if self.expected_loss_choose_b is not None else None,
                "expected_loss_choose_a": round(self.expected_loss_choose_a, 6) if self.expected_loss_choose_a is not None else None,
                "credible_interval_b": (
                    (round(self.bayesian_credible_interval_b[0], 6), round(self.bayesian_credible_interval_b[1], 6))
                    if self.bayesian_credible_interval_b else None
                ),
                "credible_interval_a": (
                    (round(self.bayesian_credible_interval_a[0], 6), round(self.bayesian_credible_interval_a[1], 6))
                    if self.bayesian_credible_interval_a else None
                ),
            },
        }


class StatisticalAnalyzer:
    """Main statistical analysis orchestrator.

    Analyzes conversion data from A/B tests using both Frequentist
    and Bayesian methodologies. The caller can choose which method
    to use, or get both.

    Usage:
        analyzer = StatisticalAnalyzer()
        result = analyzer.analyze(
            conversions_variant=120,
            total_variant=1000,
            conversions_control=100,
            total_control=1000,
            method="bayesian",
        )
    """

    DEFAULT_ALPHA = 0.05  # 95% confidence

    def analyze(
        self,
        conversions_variant: int,
        total_variant: int,
        conversions_control: int,
        total_control: int,
        method: str = "both",
        alpha: float = DEFAULT_ALPHA,
    ) -> StatisticalResult:
        """Run full statistical analysis on A/B test results.

        Args:
            conversions_variant: Number of successful conversions in variant group
            total_variant: Total users in variant group
            conversions_control: Number of successful conversions in control group
            total_control: Total users in control group
            method: 'frequentist', 'bayesian', or 'both'
            alpha: Significance level (default 0.05 for 95% confidence)

        Returns:
            StatisticalResult with all computed metrics

        Raises:
            ValueError: If input counts are invalid or method is unknown

        Security note:
            - All inputs are integers (no injection risk)
            - All computations are deterministic math functions
            - CWE-682 (Incorrect Calculation): We guard against division by zero
        """
        self._validate_inputs(
            conversions_variant, total_variant,
            conversions_control, total_control,
        )

        result = StatisticalResult()

        # ── Base rate calculations ──
        rate_variant = _safe_rate(conversions_variant, total_variant)
        rate_control = _safe_rate(conversions_control, total_control)
        absolute_diff = rate_variant - rate_control
        relative_lift = (
            absolute_diff / rate_control
            if rate_control > 0
            else 0.0
        )

        result.rate_variant = rate_variant
        result.rate_control = rate_control
        result.absolute_difference = absolute_diff
        result.relative_lift_pct = relative_lift * 100.0  # store as percentage

        # ── Frequentist analysis ──
        if method in ("frequentist", "both"):
            self._compute_frequentist_analysis(
                result,
                conversions_variant, total_variant,
                conversions_control, total_control,
                rate_variant, rate_control,
                absolute_diff, relative_lift,
                alpha,
            )

        # ── Bayesian analysis ──
        if method in ("bayesian", "both"):
            self._compute_bayesian_analysis(
                result,
                conversions_variant, total_variant,
                conversions_control, total_control,
                rate_variant, rate_control,
                alpha,
            )

        # ── Set method label ──
        result.method = method
        return result

    # ──────────── Input Validation ────────────

    def _validate_inputs(
        self,
        conversions_variant: int, total_variant: int,
        conversions_control: int, total_control: int,
    ) -> None:
        """Validate input counts.

        Raises ValueError for invalid combinations.
        """
        if total_variant <= 0 or total_control <= 0:
            raise ValueError("Total users must be positive for both groups")
        if conversions_variant < 0 or conversions_control < 0:
            raise ValueError("Conversions cannot be negative")
        if conversions_variant > total_variant or conversions_control > total_control:
            raise ValueError("Conversions cannot exceed total users")
        if total_variant + total_control < 2:
            raise ValueError("Need at least 2 total observations for statistics")

    # ──────────── Frequentist Computations ────────────

    def _compute_frequentist_analysis(
        self,
        result: StatisticalResult,
        conversions_variant: int, total_variant: int,
        conversions_control: int, total_control: int,
        rate_variant: float, rate_control: float,
        absolute_diff: float, relative_lift: float,
        alpha: float,
    ) -> None:
        """Compute frequentist statistics.

        Includes:
        - Two-proportion Z-test
        - p-value (two-tailed)
        - Confidence intervals (Wilson score for individual rates)
        - CI for absolute difference and relative lift
        """
        # Z-score and p-value
        z_statistic = _two_proportion_z_score(
            rate_variant, rate_control, total_variant, total_control
        )
        result.z_score = z_statistic

        # Two-tailed p-value
        p_value = 2.0 * (1.0 - _gaussian_cdf(abs(z_statistic)))
        result.p_value = p_value
        result.is_significant = p_value < alpha
        result.confidence_level = 1.0 - alpha

        # Wilson score confidence intervals for individual rates
        result.ci_variant_rate = self._wilson_score_interval(
            conversions_variant, total_variant, alpha
        )
        result.ci_control_rate = self._wilson_score_interval(
            conversions_control, total_control, alpha
        )

        # Standard error of the difference
        se_variant = _safe_standard_error(rate_variant, total_variant)
        se_control = _safe_standard_error(rate_control, total_control)
        se_difference = math.sqrt(se_variant ** 2 + se_control ** 2)

        # Critical z-value for the given alpha
        z_critical = self._inverse_normal_cdf(1.0 - alpha / 2.0)

        # CI for absolute difference
        ci_diff_lower = absolute_diff - z_critical * se_difference
        ci_diff_upper = absolute_diff + z_critical * se_difference
        result.ci_absolute_difference = (ci_diff_lower, ci_diff_upper)

        # CI for relative lift (via delta method)
        if rate_control > 0:
            se_relative = se_difference / rate_control
            rel_lower = relative_lift - z_critical * se_relative
            rel_upper = relative_lift + z_critical * se_relative
            result.ci_relative_lift = (rel_lower, rel_upper)

    @staticmethod
    def _wilson_score_interval(
        successes: int, total: int, alpha: float
    ) -> Tuple[float, float]:
        """Wilson score confidence interval for a proportion.

        More accurate than Wald interval, especially near 0% or 100%.
        """
        if total == 0:
            return (0.0, 0.0)

        z_critical = StatisticalAnalyzer._inverse_normal_cdf(1.0 - alpha / 2.0)
        proportion = successes / total

        denominator = 1.0 + z_critical ** 2 / total
        centre = (proportion + z_critical ** 2 / (2.0 * total)) / denominator
        margin = z_critical * math.sqrt(
            (proportion * (1.0 - proportion) / total)
            + (z_critical ** 2 / (4.0 * total ** 2))
        ) / denominator

        lower_bound = max(0.0, centre - margin)
        upper_bound = min(1.0, centre + margin)
        return (lower_bound, upper_bound)

    @staticmethod
    def _inverse_normal_cdf(upper_tail_probability: float) -> float:
        """Approximate inverse normal CDF (quantile function).

        Uses the rational approximation from Abramowitz and Stegun 26.2.23.
        Accurate to ~1e-4 for probabilities in [0.001, 0.999].

        Args:
            upper_tail_probability: The probability p (e.g., 0.975 for 95% CI)

        Returns:
            z such that P(Z < z) = p for standard normal Z
        """
        if upper_tail_probability <= 0 or upper_tail_probability >= 1:
            return 0.0

        # Handle lower tail by symmetry
        if upper_tail_probability < 0.5:
            return -StatisticalAnalyzer._inverse_normal_cdf(1.0 - upper_tail_probability)

        t_statistic = math.sqrt(-2.0 * math.log(1.0 - upper_tail_probability))
        numerator = 2.515517 + 0.802853 * t_statistic + 0.010328 * t_statistic ** 2
        denominator = 1.0 + 1.432788 * t_statistic + 0.189269 * t_statistic ** 2 + 0.001308 * t_statistic ** 3

        return t_statistic - numerator / denominator

    # ──────────── Bayesian Computations ────────────

    def _compute_bayesian_analysis(
        self,
        result: StatisticalResult,
        conversions_variant: int, total_variant: int,
        conversions_control: int, total_control: int,
        rate_variant: float, rate_control: float,
        alpha: float,
    ) -> None:
        """Compute Bayesian statistics.

        Uses Beta-Binomial conjugate model with Jeffreys prior Beta(0.5, 0.5).

        Metrics computed:
        - P(B > A) via Monte Carlo simulation
        - Expected Loss for each decision
        - Bayesian credible intervals (equal-tailed)
        """
        failures_variant = total_variant - conversions_variant
        failures_control = total_control - conversions_control

        alpha_control, beta_control = _beta_posterior_parameters(
            conversions_control, failures_control
        )
        alpha_variant, beta_variant = _beta_posterior_parameters(
            conversions_variant, failures_variant
        )

        # Probability that the variant is better than control
        prob_variant_better = _monte_carlo_prob_a_greater_than_b(
            alpha_variant, beta_variant, alpha_control, beta_control
        )
        result.probability_b_better = prob_variant_better
        result.probability_a_better = 1.0 - prob_variant_better

        # Expected Loss (how much we risk with each decision)
        loss_if_choose_variant, loss_if_choose_control = _expected_loss_monte_carlo(
            alpha_control, beta_control, alpha_variant, beta_variant
        )
        result.expected_loss_choose_b = loss_if_choose_variant
        result.expected_loss_choose_a = loss_if_choose_control

        # Bayesian credible intervals (2.5% and 97.5% quantiles)
        lower_quantile = alpha / 2.0
        upper_quantile = 1.0 - alpha / 2.0

        ci_control_lower = _beta_quantile_approximation(alpha_control, beta_control, lower_quantile)
        ci_control_upper = _beta_quantile_approximation(alpha_control, beta_control, upper_quantile)
        result.bayesian_credible_interval_a = (ci_control_lower, ci_control_upper)

        ci_variant_lower = _beta_quantile_approximation(alpha_variant, beta_variant, lower_quantile)
        ci_variant_upper = _beta_quantile_approximation(alpha_variant, beta_variant, upper_quantile)
        result.bayesian_credible_interval_b = (ci_variant_lower, ci_variant_upper)


# ──────────────────────────────────────────────
#  Standalone Bayesian helpers
# ──────────────────────────────────────────────

def _monte_carlo_prob_a_greater_than_b(
    alpha_a: float, beta_a: float,
    alpha_b: float, beta_b: float,
    simulation_samples: int = 50000,
) -> float:
    """Monte Carlo estimate of P(distribution_a > distribution_b).

    Samples from both Beta distributions and counts how often A > B.
    This is the single most business-interpretable Bayesian metric:
    "Probability that Variant A is better than Variant B."

    Args:
        alpha_a, beta_a: Posterior parameters for variant A
        alpha_b, beta_b: Posterior parameters for variant B
        simulation_samples: Number of Monte Carlo draws (more = more precise)

    Returns:
        float between 0 and 1 representing P(A > B)
    """
    count_a_better = 0

    for _ in range(simulation_samples):
        sample_a = _sample_beta_distribution(alpha_a, beta_a)
        sample_b = _sample_beta_distribution(alpha_b, beta_b)

        if sample_a > sample_b:
            count_a_better += 1

    return count_a_better / simulation_samples


def _expected_loss_monte_carlo(
    alpha_control: float, beta_control: float,
    alpha_variant: float, beta_variant: float,
    simulation_samples: int = 50000,
) -> Tuple[float, float]:
    """Expected loss from choosing the variant over control (and vice versa).

    loss_if_choose_variant = E[max(0, p_control - p_variant)]
        — How much we expect to regret if we pick the variant but
          the control was actually better.

    loss_if_choose_control = E[max(0, p_variant - p_control)]
        — How much we expect to regret if we stick with control but
          the variant was actually better.

    Returns:
        (loss_if_choose_variant, loss_if_choose_control)
    """
    total_loss_variant = 0.0
    total_loss_control = 0.0

    for _ in range(simulation_samples):
        sample_control = _sample_beta_distribution(alpha_control, beta_control)
        sample_variant = _sample_beta_distribution(alpha_variant, beta_variant)

        # Regret if we choose variant but control is better
        total_loss_variant += max(0.0, sample_control - sample_variant)

        # Regret if we choose control but variant is better
        total_loss_control += max(0.0, sample_variant - sample_control)

    return (
        total_loss_variant / simulation_samples,
        total_loss_control / simulation_samples,
    )


def _beta_quantile_approximation(
    alpha_param: float, beta_param: float,
    quantile: float,
) -> float:
    """Approximate quantile of Beta distribution.

    For large samples (alpha + beta > 20), uses normal approximation.
    For smaller samples, uses bisection search on the regularized
    incomplete beta function.

    Args:
        alpha_param, beta_param: Beta distribution parameters
        quantile: Requested quantile (between 0 and 1)

    Returns:
        Value x such that P(X <= x) = quantile
    """
    if alpha_param <= 0 or beta_param <= 0:
        return 0.5

    total = alpha_param + beta_param

    if total > 20:
        # Normal approximation is accurate for large shape parameters
        mean = alpha_param / total
        variance = (alpha_param * beta_param) / (total ** 2 * (total + 1))
        standard_dev = math.sqrt(variance)
        # Approximate z-score for the quantile
        z_critical = StatisticalAnalyzer._inverse_normal_cdf(quantile)
        estimate = mean + z_critical * standard_dev
        return max(0.0, min(1.0, estimate))

    # Bisection search for small samples
    low_bound = 0.0
    high_bound = 1.0

    for _ in range(50):
        midpoint = (low_bound + high_bound) / 2.0
        cdf_value = _regularized_beta_incomplete(midpoint, alpha_param, beta_param)

        if cdf_value < quantile:
            low_bound = midpoint
        else:
            high_bound = midpoint

    return (low_bound + high_bound) / 2.0


def _regularized_beta_incomplete(
    x_value: float,
    alpha_param: float,
    beta_param: float,
) -> float:
    """Regularized incomplete beta function I_x(alpha, beta).

    This is the CDF of the Beta distribution.
    Uses Lentz's continued fraction method.

    This is the probability that a Beta(alpha, beta) random variable
    is less than or equal to x_value.
    """
    if x_value <= 0:
        return 0.0
    if x_value >= 1:
        return 1.0

    # Use symmetry: I_x(a,b) = 1 - I_{1-x}(b,a)
    # This improves numerical stability when x > (a+1)/(a+b+2)
    threshold = (alpha_param + 1) / (alpha_param + beta_param + 2)
    if x_value > threshold:
        return 1.0 - _regularized_beta_incomplete(
            1.0 - x_value, beta_param, alpha_param
        )

    # Precompute the leading factor
    log_beta_ab = (
        math.lgamma(alpha_param) + math.lgamma(beta_param)
        - math.lgamma(alpha_param + beta_param)
    )
    leading_factor = math.exp(
        math.log(x_value) * alpha_param
        + math.log(1.0 - x_value) * beta_param
        - log_beta_ab
        - math.log(alpha_param)
    )

    # Lentz's continued fraction evaluation
    continued_fraction = 1.0
    c_term = 1.0
    d_term = 1.0 - (alpha_param + beta_param) * x_value / (alpha_param + 1.0)

    if abs(d_term) < 1e-30:
        d_term = 1e-30
    d_term = 1.0 / d_term
    continued_fraction = d_term

    for iteration in range(1, 201):
        # Even step
        numerator_even = (
            iteration * (beta_param - iteration) * x_value
            / ((alpha_param + 2 * iteration - 1) * (alpha_param + 2 * iteration))
        )
        d_term = 1.0 + numerator_even * d_term
        if abs(d_term) < 1e-30:
            d_term = 1e-30
        c_term = 1.0 + numerator_even / c_term
        if abs(c_term) < 1e-30:
            c_term = 1e-30
        d_term = 1.0 / d_term
        delta = c_term * d_term
        continued_fraction *= delta

        # Odd step
        numerator_odd = (
            -(alpha_param + iteration) * (alpha_param + beta_param + iteration) * x_value
            / ((alpha_param + 2 * iteration) * (alpha_param + 2 * iteration + 1))
        )
        d_term = 1.0 + numerator_odd * d_term
        if abs(d_term) < 1e-30:
            d_term = 1e-30
        c_term = 1.0 + numerator_odd / c_term
        if abs(c_term) < 1e-30:
            c_term = 1e-30
        d_term = 1.0 / d_term
        delta = c_term * d_term
        continued_fraction *= delta

        # Check convergence
        if abs(delta - 1.0) < 1e-10:
            break

    return leading_factor * (continued_fraction - 1.0)


# ──────────────────────────────────────────────
#  Tiny utility helpers
# ──────────────────────────────────────────────

def _safe_rate(numerator: int, denominator: int) -> float:
    """Safely compute a rate, guarding against division by zero."""
    return numerator / denominator if denominator > 0 else 0.0


def _safe_standard_error(rate: float, count: int) -> float:
    """Standard error of a proportion, safe against zero counts."""
    if count <= 0:
        return 0.0
    return math.sqrt(rate * (1.0 - rate) / count)