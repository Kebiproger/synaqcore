// Сценарий mixed: 90% выдача вариантов / 10% события (случайный выбор на итерацию).
import { buildOptions, getVariant, postEvent, sanityCheck } from './common.js';

export const options = buildOptions(['variants', 'tracker']);
export function setup() { sanityCheck(); }
export default function () {
  if (Math.random() < 0.1) postEvent(); else getVariant();
}
