// Сценарий tracker: POST /api/v1/tracker/events с валидным событием.
import { buildOptions, postEvent, sanityCheck } from './common.js';

export const options = buildOptions(['tracker']);
export function setup() { sanityCheck(); }
export default function () { postEvent(); }
