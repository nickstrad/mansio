//go:build race

package sim_test

// Under the race detector SQLite runs several times slower: fewer default seeds.
const defaultSeeds, determinismSeeds = 3, 2
