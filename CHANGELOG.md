# [0.12.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.11.0...v0.12.0) (2026-11-27)


### Features

* ManagementCluster spec.allowedKinds limits the XR kinds a cluster takes orders for ([#39](https://github.com/stuttgart-things/machinery-order-service/issues/39)) ([#40](https://github.com/stuttgart-things/machinery-order-service/issues/40)) ([7fbdda5](https://github.com/stuttgart-things/machinery-order-service/commit/7fbdda5986e61fbd962d8665a5c68e26b318cbba)), closes [#25](https://github.com/stuttgart-things/machinery-order-service/issues/25)

# [0.11.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.10.0...v0.11.0) (2026-11-25)


### Features

* refuse an XR whose environmentConfig matches no EnvironmentConfig label ([#35](https://github.com/stuttgart-things/machinery-order-service/issues/35)) ([247e744](https://github.com/stuttgart-things/machinery-order-service/commit/247e7446cc39e4db98d44e6d1dbc69d3b4cd80b8)), closes [#32](https://github.com/stuttgart-things/machinery-order-service/issues/32)

# [0.10.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.9.0...v0.10.0) (2026-11-25)


### Features

* ManagementCluster capabilities, refuse orders for kinds the cluster does not offer ([#34](https://github.com/stuttgart-things/machinery-order-service/issues/34)) ([269311f](https://github.com/stuttgart-things/machinery-order-service/commit/269311fd18081746b450438ddbf2dadda52dd5dc))

# [0.9.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.8.0...v0.9.0) (2026-11-25)


### Features

* ManagementCluster health through machinery, not-ready clusters refuse orders ([#33](https://github.com/stuttgart-things/machinery-order-service/issues/33)) ([4c86899](https://github.com/stuttgart-things/machinery-order-service/commit/4c86899b47f6afbb7af47b8982ebfabec0ac8ea0))

# [0.8.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.7.0...v0.8.0) (2026-11-24)


### Features

* gRPC API for orders ([#24](https://github.com/stuttgart-things/machinery-order-service/issues/24)) ([0fd8bce](https://github.com/stuttgart-things/machinery-order-service/commit/0fd8bce5982337c785b516d88eadbb003a1af3d6))
* maximum lifetime for auto-merge orders ([#27](https://github.com/stuttgart-things/machinery-order-service/issues/27)) ([b58453e](https://github.com/stuttgart-things/machinery-order-service/commit/b58453e5cbda343e48a98e3712876aec39a02f9b))

# [0.7.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.6.0...v0.7.0) (2026-11-23)


### Features

* per-cluster claim-machinery-api and parameter prefixes in auto-merge rules ([#26](https://github.com/stuttgart-things/machinery-order-service/issues/26)) ([9533967](https://github.com/stuttgart-things/machinery-order-service/commit/953396707e22204103df3039dd23cc14d75a3e56)), closes [stuttgart-things/stuttgart-things#4828](https://github.com/stuttgart-things/stuttgart-things/issues/4828)

# [0.6.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.5.0...v0.6.0) (2026-11-22)


### Features

* **ui:** all orders view, filtered and grouped by group ([5c4b8ce](https://github.com/stuttgart-things/machinery-order-service/commit/5c4b8ce0331472e7a7ac2022f408d2df0c4485bf))

# [0.5.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.4.1...v0.5.0) (2026-11-22)


### Features

* teardown as a delete-claim pull request, delete order branches after merge ([ae0de36](https://github.com/stuttgart-things/machinery-order-service/commit/ae0de361f773ba6515cc477925783d022a389244)), closes [#19](https://github.com/stuttgart-things/machinery-order-service/issues/19) [#20](https://github.com/stuttgart-things/machinery-order-service/issues/20) [#19](https://github.com/stuttgart-things/machinery-order-service/issues/19) [#20](https://github.com/stuttgart-things/machinery-order-service/issues/20)

## [0.4.1](https://github.com/stuttgart-things/machinery-order-service/compare/v0.4.0...v0.4.1) (2026-11-19)


### Bug Fixes

* **controller:** auto-merge state in status.message ([20f0aa3](https://github.com/stuttgart-things/machinery-order-service/commit/20f0aa357ef22288f14748e603771c854e7c2e99))

# [0.4.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.3.0...v0.4.0) (2026-11-19)


### Features

* auto-merge when the order asks for it and a rule allows it ([40976db](https://github.com/stuttgart-things/machinery-order-service/commit/40976db3854e1751aaeac6824ea983c1630dc549))

# [0.3.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.2.0...v0.3.0) (2026-11-19)


### Features

* **api:** order visibility, management clusters, accepted status ([673eab8](https://github.com/stuttgart-things/machinery-order-service/commit/673eab8d3d9bde0dea424adf2c5995d99d4758b8)), closes [#12](https://github.com/stuttgart-things/machinery-order-service/issues/12) [#13](https://github.com/stuttgart-things/machinery-order-service/issues/13) [#14](https://github.com/stuttgart-things/machinery-order-service/issues/14) [#12](https://github.com/stuttgart-things/machinery-order-service/issues/12) [#13](https://github.com/stuttgart-things/machinery-order-service/issues/13) [#14](https://github.com/stuttgart-things/machinery-order-service/issues/14)

# [0.2.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.1.3...v0.2.0) (2026-11-16)


### Features

* **web:** orders fragment for the sthings-portal overview ([307bddf](https://github.com/stuttgart-things/machinery-order-service/commit/307bddfc02e0dc861abad81f59b544e316d4e32c))

## [0.1.3](https://github.com/stuttgart-things/machinery-order-service/compare/v0.1.2...v0.1.3) (2026-11-16)


### Bug Fixes

* pass the orderer to the render as requested_by ([f4b1cac](https://github.com/stuttgart-things/machinery-order-service/commit/f4b1cacbacd17e4a20674e8bce0616fe2f198ac7)), closes [#5](https://github.com/stuttgart-things/machinery-order-service/issues/5)
* split machinery's connection details into address and info ([0546bec](https://github.com/stuttgart-things/machinery-order-service/commit/0546bec884fa4eabbe90a8b187eba802a0448ba9)), closes [#6](https://github.com/stuttgart-things/machinery-order-service/issues/6)

## [0.1.2](https://github.com/stuttgart-things/machinery-order-service/compare/v0.1.1...v0.1.2) (2026-11-16)


### Bug Fixes

* **controller:** phase Deleted when a Ready XR is gone, keep the last live status ([0eb7459](https://github.com/stuttgart-things/machinery-order-service/commit/0eb74594b0bb1b3eed7b14b8929af9dc9d5f2ac8)), closes [stuttgart-things/stuttgart-things#4297](https://github.com/stuttgart-things/stuttgart-things/issues/4297) [stuttgart-things/stuttgart-things#4557](https://github.com/stuttgart-things/stuttgart-things/issues/4557)

## [0.1.1](https://github.com/stuttgart-things/machinery-order-service/compare/v0.1.0...v0.1.1) (2026-11-15)


### Bug Fixes

* **controller:** default flux.revisionField to the watch config's label Revision ([4a63ba9](https://github.com/stuttgart-things/machinery-order-service/commit/4a63ba98d2cbf03fd961d172bc56c5e8354cb298))
* **dagger:** build-image with the go.mod toolchain and the ko ldflag variables ([9fc0f95](https://github.com/stuttgart-things/machinery-order-service/commit/9fc0f95145ea1d7218dacf62cd844b026bea92fc))
* **deps:** golang.org/x/net v0.62.0 (CVE-2026-78661) ([b769cea](https://github.com/stuttgart-things/machinery-order-service/commit/b769cea6c36e1dd5218e82621e3386fc12780cd0))
* **release:** render the kustomize base from a neutral profile ([e13db2c](https://github.com/stuttgart-things/machinery-order-service/commit/e13db2ca974993a113103b8b70bd332286c1c4c8))

# [0.1.0](https://github.com/stuttgart-things/machinery-order-service/compare/v0.0.0...v0.1.0) (2026-11-14)


### Bug Fixes

* **dagger:** rename envtestK8sVersion, Dagger looked it up as envtestK8SVersion ([5d74b25](https://github.com/stuttgart-things/machinery-order-service/commit/5d74b252534ce9c6b16077031d216e3909da8b9d))


### Features

* deployment (KCL), Dagger module, image and release workflows ([9732535](https://github.com/stuttgart-things/machinery-order-service/commit/973253595e166641de28da18d5cd71bf38a9a1f5))
* skeleton - Order/ManagementCluster CRDs, controller, git delivery, REST/SSE API, UI stage 1 ([2256139](https://github.com/stuttgart-things/machinery-order-service/commit/2256139747643fbe8ee39c1dc3612405327bb2fc)), closes [stuttgart-things/stuttgart-things#4297](https://github.com/stuttgart-things/stuttgart-things/issues/4297) [#4300](https://github.com/stuttgart-things/machinery-order-service/issues/4300)
