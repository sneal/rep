package internal_test

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/cloudfoundry-incubator/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/bbs/models/test/model_helpers"
	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/rep/evacuation/evacuation_context/fake_evacuation_context"
	"github.com/cloudfoundry-incubator/rep/generator/internal"
	"github.com/cloudfoundry-incubator/rep/generator/internal/fake_internal"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
)

var _ = Describe("OrdinaryLRPProcessor", func() {
	const expectedCellID = "cell-id"

	var (
		processor          internal.LRPProcessor
		logger             *lagertest.TestLogger
		bbsClient          *fake_bbs.FakeClient
		containerDelegate  *fake_internal.FakeContainerDelegate
		evacuationReporter *fake_evacuation_context.FakeEvacuationReporter
	)

	BeforeEach(func() {
		bbsClient = new(fake_bbs.FakeClient)
		containerDelegate = new(fake_internal.FakeContainerDelegate)
		evacuationReporter = &fake_evacuation_context.FakeEvacuationReporter{}
		evacuationReporter.EvacuatingReturns(false)
		processor = internal.NewLRPProcessor(bbsClient, containerDelegate, expectedCellID, evacuationReporter, 124)
		logger = lagertest.NewTestLogger("test")
	})

	Describe("Process", func() {
		const sessionPrefix = "test.ordinary-lrp-processor."

		var (
			desiredLRP          *models.DesiredLRP
			expectedLrpKey      models.ActualLRPKey
			expectedInstanceKey models.ActualLRPInstanceKey
			expectedNetInfo     models.ActualLRPNetInfo
			expectedSessionName string
		)

		BeforeEach(func() {
			desiredLRP = model_helpers.NewValidDesiredLRP("process-guid")
			expectedLrpKey = models.NewActualLRPKey("process-guid", 2, "domain")
			expectedInstanceKey = models.NewActualLRPInstanceKey("instance-guid", "cell-id")
			expectedNetInfo = models.NewActualLRPNetInfo("1.2.3.4", models.NewPortMapping(61999, 8080))
		})

		Context("when given an LRP container", func() {
			var container executor.Container

			BeforeEach(func() {
				container = newLRPContainer(expectedLrpKey, expectedInstanceKey, expectedNetInfo)
			})

			JustBeforeEach(func() {
				processor.Process(logger, container)
			})

			Context("and the container is INVALID", func() {
				BeforeEach(func() {
					expectedSessionName = sessionPrefix + "process-invalid-container"
					container.State = executor.StateInvalid
				})

				It("logs an error", func() {
					Expect(logger).To(Say(expectedSessionName))
				})
			})

			Context("and the container is RESERVED", func() {
				BeforeEach(func() {
					bbsClient.DesiredLRPByProcessGuidReturns(desiredLRP, nil)
					expectedSessionName = sessionPrefix + "process-reserved-container"
					container.State = executor.StateReserved
				})

				It("claims the actualLRP in the bbs", func() {
					Expect(bbsClient.ClaimActualLRPCallCount()).To(Equal(1))
					processGuid, index, instanceKey := bbsClient.ClaimActualLRPArgsForCall(0)
					Expect(processGuid).To(Equal(expectedLrpKey.ProcessGuid))
					Expect(int32(index)).To(Equal(expectedLrpKey.Index))
					Expect(*instanceKey).To(Equal(expectedInstanceKey))
				})

				Context("when claiming fails because ErrActualLRPCannotBeClaimed", func() {
					BeforeEach(func() {
						bbsClient.ClaimActualLRPReturns(models.NewError(
							models.Error_ActualLRPCannotBeClaimed,
							"something-broke?",
						))
					})

					It("deletes the container", func() {
						Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
						delegateLogger, containerGuid := containerDelegate.DeleteContainerArgsForCall(0)
						Expect(containerGuid).To(Equal(container.Guid))
						Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
					})

					It("does not try to run the container", func() {
						Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					})
				})

				Context("when claiming fails for an unknown reason", func() {
					BeforeEach(func() {
						bbsClient.ClaimActualLRPReturns(errors.New("boom"))
					})

					It("does not delete the container", func() {
						Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
					})

					It("does not try to run the container", func() {
						Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					})
				})

				Context("when claiming succeeds", func() {
					It("runs the container", func() {
						Expect(containerDelegate.RunContainerCallCount()).To(Equal(1))

						expectedRunRequest, err := rep.NewRunRequestFromDesiredLRP(container.Guid, desiredLRP, &expectedLrpKey, &expectedInstanceKey)
						Expect(err).NotTo(HaveOccurred())

						delegateLogger, runRequest := containerDelegate.RunContainerArgsForCall(0)
						Expect(*runRequest).To(Equal(expectedRunRequest))
						Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
					})

					Context("when running fails", func() {
						BeforeEach(func() {
							containerDelegate.RunContainerReturns(false)
						})

						It("removes the actual LRP", func() {
							Expect(bbsClient.RemoveActualLRPCallCount()).To(Equal(1))
							processGuid, index := bbsClient.RemoveActualLRPArgsForCall(0)

							Expect(processGuid).To(Equal(expectedLrpKey.ProcessGuid))
							Expect(int32(index)).To(Equal(expectedLrpKey.Index))
						})
					})
				})

				var itClaimsTheLRPOrDeletesTheContainer = func(expectedSessionName string) {
					It("claims the lrp", func() {
						Expect(bbsClient.ClaimActualLRPCallCount()).To(Equal(1))
						processGuid, index, instanceKey := bbsClient.ClaimActualLRPArgsForCall(0)
						Expect(processGuid).To(Equal(expectedLrpKey.ProcessGuid))
						Expect(int32(index)).To(Equal(expectedLrpKey.Index))
						Expect(*instanceKey).To(Equal(expectedInstanceKey))
					})

					Context("when the claim fails because ErrActualLRPCannotBeClaimed", func() {
						BeforeEach(func() {
							bbsClient.ClaimActualLRPReturns(models.ErrActualLRPCannotBeClaimed)
						})

						It("deletes the container", func() {
							Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
							delegateLogger, containerGuid := containerDelegate.DeleteContainerArgsForCall(0)
							Expect(containerGuid).To(Equal(container.Guid))
							Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
						})
					})

					Context("when the claim fails for an unknown reason", func() {
						BeforeEach(func() {
							bbsClient.ClaimActualLRPReturns(errors.New("boom"))
						})

						It("does not stop or delete the container", func() {
							Expect(containerDelegate.StopContainerCallCount()).To(Equal(0))
							Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
						})
					})
				}

				Context("and the container is INITIALIZING", func() {
					BeforeEach(func() {
						container.State = executor.StateInitializing
					})

					itClaimsTheLRPOrDeletesTheContainer(sessionPrefix + "process-initializing-container")
				})

				Context("and the container is CREATED", func() {
					BeforeEach(func() {
						container.State = executor.StateCreated
					})

					itClaimsTheLRPOrDeletesTheContainer(sessionPrefix + "process-created-container")
				})

				Context("and the container is RUNNING", func() {
					BeforeEach(func() {
						expectedSessionName = sessionPrefix + "process-running-container"
						container.State = executor.StateRunning
						container.ExternalIP = "1.2.3.4"
						container.Ports = []executor.PortMapping{{ContainerPort: 8080, HostPort: 61999}}
					})

					It("starts the lrp", func() {
						Expect(bbsClient.StartActualLRPCallCount()).To(Equal(1))
						lrpKey, instanceKey, netInfo := bbsClient.StartActualLRPArgsForCall(0)
						Expect(*lrpKey).To(Equal(expectedLrpKey))
						Expect(*instanceKey).To(Equal(expectedInstanceKey))
						Expect(*netInfo).To(Equal(expectedNetInfo))

						Eventually(logger).Should(Say(
							fmt.Sprintf(
								`"net_info":\{"address":"%s","ports":\[\{"container_port":%d,"host_port":%d\}\]\}`,
								expectedNetInfo.Address,
								expectedNetInfo.Ports[0].ContainerPort,
								expectedNetInfo.Ports[0].HostPort,
							),
						))
					})

					Context("when starting fails because ErrActualLRPCannotBeStarted", func() {
						BeforeEach(func() {
							bbsClient.StartActualLRPReturns(models.NewError(models.Error_ActualLRPCannotBeStarted, "foobar").ToError())
						})

						It("stops the container", func() {
							Expect(containerDelegate.StopContainerCallCount()).To(Equal(1))
							delegateLogger, containerGuid := containerDelegate.StopContainerArgsForCall(0)
							Expect(containerGuid).To(Equal(container.Guid))
							Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
						})
					})

					Context("when starting fails for an unknown reason", func() {
						BeforeEach(func() {
							bbsClient.StartActualLRPReturns(errors.New("boom"))
						})

						It("does not stop or delete the container", func() {
							Expect(containerDelegate.StopContainerCallCount()).To(Equal(0))
							Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
						})
					})
				})

				Context("and the container is COMPLETED", func() {
					BeforeEach(func() {
						expectedSessionName = sessionPrefix + "process-completed-container"
						container.State = executor.StateCompleted
					})

					Context("and the container was requested to stop", func() {
						BeforeEach(func() {
							container.RunResult.Stopped = true
						})

						It("removes the actual LRP", func() {
							Expect(bbsClient.RemoveActualLRPCallCount()).To(Equal(1))
							processGuid, index := bbsClient.RemoveActualLRPArgsForCall(0)

							Expect(processGuid).To(Equal(expectedLrpKey.ProcessGuid))
							Expect(int32(index)).To(Equal(expectedLrpKey.Index))
						})

						Context("when the removal succeeds", func() {
							It("deletes the container", func() {
								Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
								delegateLogger, containerGuid := containerDelegate.DeleteContainerArgsForCall(0)
								Expect(containerGuid).To(Equal(container.Guid))
								Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
							})
						})

						Context("when the removal fails", func() {
							BeforeEach(func() {
								bbsClient.RemoveActualLRPReturns(errors.New("whoops"))
							})

							It("deletes the container", func() {
								Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
								delegateLogger, containerGuid := containerDelegate.DeleteContainerArgsForCall(0)
								Expect(containerGuid).To(Equal(container.Guid))
								Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
							})
						})
					})

					Context("and the container was not requested to stop", func() {
						BeforeEach(func() {
							container.RunResult.Stopped = false
							container.RunResult.FailureReason = "crashed"
						})

						It("crashes the actual LRP", func() {
							Expect(bbsClient.CrashActualLRPCallCount()).To(Equal(1))
							lrpKey, instanceKey, reason := bbsClient.CrashActualLRPArgsForCall(0)
							Expect(*lrpKey).To(Equal(expectedLrpKey))
							Expect(*instanceKey).To(Equal(expectedInstanceKey))
							Expect(reason).To(Equal("crashed"))
						})

						It("deletes the container", func() {
							Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
							delegateLogger, containerGuid := containerDelegate.DeleteContainerArgsForCall(0)
							Expect(containerGuid).To(Equal(container.Guid))
							Expect(delegateLogger.SessionName()).To(Equal(expectedSessionName))
						})
					})
				})

				Context("and the container is in an invalid state", func() {
					BeforeEach(func() {
						container.State = executor.StateInvalid
					})

					It("logs the container as a warning", func() {
						Expect(logger).To(Say(sessionPrefix + "process-invalid-container.not-processing-container-in-invalid-state"))
					})
				})
			})
		})
	})
})

func newLRPContainer(lrpKey models.ActualLRPKey, instanceKey models.ActualLRPInstanceKey, netInfo models.ActualLRPNetInfo) executor.Container {
	ports := []executor.PortMapping{}
	for _, portMap := range netInfo.Ports {
		ports = append(ports, executor.PortMapping{
			ContainerPort: uint16(portMap.ContainerPort),
			HostPort:      uint16(portMap.HostPort),
		})
	}

	return executor.Container{
		Guid: rep.LRPContainerGuid(lrpKey.ProcessGuid, instanceKey.InstanceGuid),
		RunInfo: executor.RunInfo{
			Action: models.WrapAction(&models.RunAction{Path: "true"}),
			Ports:  ports,
		},
		ExternalIP: netInfo.Address,
		Tags: executor.Tags{
			rep.ProcessGuidTag:  lrpKey.ProcessGuid,
			rep.InstanceGuidTag: instanceKey.InstanceGuid,
			rep.ProcessIndexTag: strconv.Itoa(int(lrpKey.Index)),
			rep.DomainTag:       lrpKey.Domain,
		},
	}
}
