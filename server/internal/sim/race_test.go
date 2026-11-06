//go:build race

package sim_test

// Under the race detector SQLite runs several times slower: fewer default seeds.
const defaultSeeds, determinismSeeds, defaultIRSeeds = 3, 2, 1
