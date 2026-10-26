// Сценарий variants: GET /api/v1/variants/{experiment}?user_id=<случайный>.
import { buildOptions, getVariant, sanityCheck } from './common.js';

export const options = buildOptions(['variants']);
export function setup() { sanityCheck(); }
export default function () { getVariant(); }
