import fc from "fast-check";
export { SEED, titled } from "../../property/arbitraries.ts";
import { SEED } from "../../property/arbitraries.ts";

export const scenarios = [
  "documents",
  "queries",
  "backup_restore",
  "tenant_isolation",
  "auth",
  "log_analysis",
  "admin_lifecycle",
  "scale_out",
] as const;
export const order = fc.sample(
  fc.shuffledSubarray([...scenarios], {
    minLength: scenarios.length,
    maxLength: scenarios.length,
  }),
  { seed: SEED, numRuns: 1 },
)[0];
const word = fc.string({
  unit: fc.constantFrom(..."abcdefghijklmnopqrstuvwxyz"),
  minLength: 6,
  maxLength: 10,
});
export const names = fc.sample(word, { seed: SEED, numRuns: 16 }).map((s, i) => `${s}-${i}`);
export const players = { xPlayer: names[0], oPlayer: names[1] };
// Legal X win, O win, and draw, transformed by a seeded board symmetry.
const symmetry = fc.sample(fc.integer({ min: 0, max: 7 }), { seed: SEED, numRuns: 1 })[0];
function transform(cell: number): number {
  let x = cell % 3, y = Math.floor(cell / 3);
  if (symmetry >= 4) x = 2 - x;
  for (let i = 0; i < symmetry % 4; i++) [x, y] = [2 - y, x];
  return y * 3 + x;
}
export const moves = {
  X: [0, 3, 1, 4, 2].map(transform),
  O: [0, 3, 1, 4, 8, 5].map(transform),
  draw: [0, 1, 2, 4, 3, 5, 7, 6, 8].map(transform),
};
const unfinished = fc.sample(fc.integer({ min: 1, max: 4 }), { seed: SEED, numRuns: 3 });
export const workloads = [moves.X, moves.O, moves.draw, ...unfinished.map((n) => moves.X.slice(0, n))];
