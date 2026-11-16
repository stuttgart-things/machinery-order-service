## [0.1.2](https://github.com/stuttgart-things/machinery-order-service/compare/v0.1.1...v0.1.2) (2026-11-16)


### Bug Fixes

* **controller:** phase Deleted when a Ready XR is gone, keep the last live status ([0eb7459](https://github.com/stuttgart-things/machinery-order-service/commit/0eb74594b0bb1b3eed7b14b8929af9dc9d5f2ac8)), closes [stuttgart-things/stuttgart-things#4297](https://github.com/stuttgart-things/stuttgart-things/issues/4297) [#4557](https://github.com/stuttgart-things/machinery-order-service/issues/4557)

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
