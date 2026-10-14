export interface Experiment {
  name: string;
  variants: Record<string, number>;
  config?: Record<string, unknown> | null;
  url?: string | null;
  is_active: boolean;
  total_weight?: number;
}

export interface ExperimentCreate {
  name: string;
  variants: Record<string, number>;
  config?: Record<string, unknown> | null;
  url?: string | null;
  is_active: boolean;
}

export type ExperimentPatch = Partial<Pick<ExperimentCreate, "is_active" | "variants" | "config" | "url">>;

export interface VariantStats {
  total_users: number | string;
  converting_users: number | string;
  conversion_rate: number | string;
}

export interface Recommendation {
  type?: string;
  summary?: string;
  action?: string;
  confidence?: string | number;
  risk_note?: string | null;
  additional_insights?: string[];
}

export interface Frequentist {
  p_value: number | null;
  is_significant: boolean | null;
  confidence_level?: number | null;
  z_score?: number | null;
  ci_variant_rate?: [number, number] | null;
  ci_control_rate?: [number, number] | null;
  ci_absolute_difference?: [number, number] | null;
  ci_relative_lift?: [number, number] | null;
}

export interface Bayesian {
  probability_b_better: number | null;
  probability_a_better: number | null;
  expected_loss_choose_b?: number | null;
  expected_loss_choose_a?: number | null;
  credible_interval_b?: [number, number] | null;
  credible_interval_a?: [number, number] | null;
}

export interface Statistical {
  method?: string;
  sample_rates?: {
    rate_variant: number;
    rate_control: number;
    absolute_difference: number;
    relative_lift_pct: number | null;
  };
  frequentist?: Frequentist | null;
  bayesian?: Bayesian | null;
  error?: string;
}

export interface DailyTrendRow {
  variant: string;
  day: string;
  daily_users: number | string;
  daily_conversions: number | string;
  daily_conversion_rate_pct: number | string;
}

export interface StatsResponse {
  experiment: string;
  goal_event: string;
  method: string;
  variants: Record<string, VariantStats>;
  recommendations?: Recommendation[];
  statistical?: Statistical;
  business?: unknown;
  segmentation?: unknown;
  funnel?: unknown;
  outliers?: unknown;
  daily_trend?: DailyTrendRow[];
}

export interface BasicStats {
  experiment: string;
  summary: {
    variant: string;
    total_events: number | string;
    unique_users: number | string;
    first_event: string;
    last_event: string;
  }[];
  by_event?: unknown;
  funnel?: { variant: string; views: number | string; clicks: number | string; conversion_pct: number | string }[];
  recent?: unknown;
}
