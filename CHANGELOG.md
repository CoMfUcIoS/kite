# Changelog

## [0.9.1](https://github.com/CoMfUcIoS/kite/compare/v0.9.0...v0.9.1) (2026-09-30)


### Bug Fixes

* explain the --json shape and the CI +N suffix in --help ([#21](https://github.com/CoMfUcIoS/kite/issues/21)) ([16fb522](https://github.com/CoMfUcIoS/kite/commit/16fb522ee4750fd3dd189bbf9e4fa9464157f48d))

## [0.9.0](https://github.com/CoMfUcIoS/kite/compare/v0.8.0...v0.9.0) (2026-09-30)


### Features

* show which directories prune would remove ([#19](https://github.com/CoMfUcIoS/kite/issues/19)) ([c821691](https://github.com/CoMfUcIoS/kite/commit/c821691ad2265514357df2b9f90439245bb57eab))


### Bug Fixes

* mark PR sub-rows with ↳ so they don't look like worktrees ([#17](https://github.com/CoMfUcIoS/kite/issues/17)) ([c516340](https://github.com/CoMfUcIoS/kite/commit/c516340545531ed1bdb3d6dae9dac73eb7b1351a))

## [0.8.0](https://github.com/CoMfUcIoS/kite/compare/v0.7.0...v0.8.0) (2026-09-30)


### Features

* show progress while kite works, and how long it took ([#16](https://github.com/CoMfUcIoS/kite/issues/16)) ([2932a2e](https://github.com/CoMfUcIoS/kite/commit/2932a2e86a22d1b85b1c5a55db40bb1e6ad7bb2c))


### Performance Improvements

* speed up status with the untracked cache and an early review search ([#14](https://github.com/CoMfUcIoS/kite/issues/14)) ([0380482](https://github.com/CoMfUcIoS/kite/commit/03804822fb71e01228bb13e2849eec27beb4888d))

## [0.7.0](https://github.com/CoMfUcIoS/kite/compare/v0.6.0...v0.7.0) (2026-09-30)


### Features

* find squash-merged branches whose remote branch was kept ([#12](https://github.com/CoMfUcIoS/kite/issues/12)) ([16c9c30](https://github.com/CoMfUcIoS/kite/commit/16c9c3001f441906e239c662c1666a86e3bab7b8))

## [0.6.0](https://github.com/CoMfUcIoS/kite/compare/v0.5.0...v0.6.0) (2026-09-30)


### Features

* add --json output to status, update, stash and prune ([#10](https://github.com/CoMfUcIoS/kite/issues/10)) ([1bc25fb](https://github.com/CoMfUcIoS/kite/commit/1bc25fb57ada0f28abfc4c9c01bb98f099b30018))
* flag branches whose upstream was force-pushed ([#11](https://github.com/CoMfUcIoS/kite/issues/11)) ([2fb80a7](https://github.com/CoMfUcIoS/kite/commit/2fb80a765d5359428ee21ef2ca8f79cf1a3e0bc1))
* let prune remove finished worktrees ([#8](https://github.com/CoMfUcIoS/kite/issues/8)) ([2c16983](https://github.com/CoMfUcIoS/kite/commit/2c16983086bcd354dfc5c0fc28b5003ece4ee61f))

## [0.5.0](https://github.com/CoMfUcIoS/kite/compare/v0.4.0...v0.5.0) (2026-09-30)


### Features

* add claude-code skill and fix gitignore ([3037e71](https://github.com/CoMfUcIoS/kite/commit/3037e71719a136c7de5c858e588762aef8e48e6e))
* follow linked worktrees outside the root ([#6](https://github.com/CoMfUcIoS/kite/issues/6)) ([446f724](https://github.com/CoMfUcIoS/kite/commit/446f724f44c202ba1dcea7aa09723f7523b67968))
* report shared worktree state once per repo ([#5](https://github.com/CoMfUcIoS/kite/issues/5)) ([618ba3a](https://github.com/CoMfUcIoS/kite/commit/618ba3a6e2d5e76dc2a45a5aa04d13a9fcc70a3a))


### Bug Fixes

* count worktrees apart from repos in the footer ([6a683d3](https://github.com/CoMfUcIoS/kite/commit/6a683d3b21fa2d0bffd092d1c785b83315a1d704))
* update each shared git dir once and mark worktree rows ([#7](https://github.com/CoMfUcIoS/kite/issues/7)) ([e933550](https://github.com/CoMfUcIoS/kite/commit/e9335503354288b15d99e822fced09d3219cd33d))

## [0.4.0](https://github.com/CoMfUcIoS/kite/compare/v0.3.0...v0.4.0) (2026-09-10)


### Features

* surface every open PR, its review state and conflict risk ([8f18714](https://github.com/CoMfUcIoS/kite/commit/8f18714edd5800b88552971dbbc61dc297a03c6e))

## [0.3.0](https://github.com/CoMfUcIoS/kite/compare/v0.2.0...v0.3.0) (2026-08-20)


### Features

* add prune, stash and path commands ([a75d2ea](https://github.com/CoMfUcIoS/kite/commit/a75d2ea64fc2e96701b91de1b0736999bd831fed))

## 0.2.0 (2026-08-20)


### Features

* add --version flag ([cc2fd3a](https://github.com/CoMfUcIoS/kite/commit/cc2fd3a23235621cfb34f39f2986288b06df53a3))
* add curl installer and release binaries ([48d4e10](https://github.com/CoMfUcIoS/kite/commit/48d4e10cc7165c2f31d6217a05400054c2761222))
