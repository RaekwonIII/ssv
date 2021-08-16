package ibft

import (
	"encoding/json"
	"errors"
	"github.com/herumi/bls-eth-go-binary/bls"
	"math"
	"time"

	"go.uber.org/zap"

	"github.com/bloxapp/ssv/ibft/msgcont"
	"github.com/bloxapp/ssv/ibft/pipeline"
	"github.com/bloxapp/ssv/ibft/pipeline/auth"
	"github.com/bloxapp/ssv/ibft/pipeline/changeround"
	"github.com/bloxapp/ssv/ibft/proto"
)

func (i *Instance) changeRoundMsgValidationPipeline() pipeline.Pipeline {
	return pipeline.Combine(
		auth.BasicMsgValidation(),
		auth.MsgTypeCheck(proto.RoundState_ChangeRound),
		auth.ValidateLambdas(i.State.Lambda.Get()),
		auth.ValidateSequenceNumber(i.State.SeqNumber),
		auth.AuthorizeMsg(i.ValidatorShare),
		changeround.Validate(i.ValidatorShare),
	)
}

func (i *Instance) changeRoundFullQuorumMsgPipeline() pipeline.Pipeline {
	return pipeline.Combine(
		i.changeRoundMsgValidationPipeline(),
		auth.ValidateRound(i.Round()),
		changeround.AddChangeRoundMessage(i.Logger, i.ChangeRoundMessages, i.State),
		i.uponChangeRoundFullQuorum(),
	)
}

/**
upon receiving a quorum Qrc of valid ⟨ROUND-CHANGE, λi, ri, −, −⟩ messages such that
	leader(λi, ri) = pi ∧ JustifyRoundChange(Qrc) do
		if HighestPrepared(Qrc) ̸= ⊥ then
			let v such that (−, v) = HighestPrepared(Qrc))
		else
			let v such that v = inputValue i
		broadcast ⟨PRE-PREPARE, λi, ri, v⟩
*/
func (i *Instance) uponChangeRoundFullQuorum() pipeline.Pipeline {
	return pipeline.WrapFunc("upon change round full quorum", func(signedMessage *proto.SignedMessage) error {
		var err error
		quorum, msgsCount, committeeSize := i.changeRoundQuorum(signedMessage.Message.Round)
		justifyRound, err := i.JustifyRoundChange(signedMessage.Message.Round)
		if err != nil {
			return err
		}
		isLeader := i.IsLeader()

		// change round if quorum reached
		if !quorum {
			i.Logger.Info("change round - quorum not reached", zap.Uint64("round", signedMessage.Message.Round), zap.Int("msgsCount", msgsCount), zap.Int("committeeSize", committeeSize))
			return nil
		}

		i.processChangeRoundQuorumOnce.Do(func() {
			i.ProcessStageChange(proto.RoundState_PrePrepare)
			i.Logger.Info("change round quorum received.",
				zap.Uint64("round", signedMessage.Message.Round),
				zap.Bool("is_leader", isLeader),
				zap.Bool("round_justified", justifyRound))

			if !isLeader {
				return
			}

			if !justifyRound {
				err = errors.New("could not justify round change: tried to broadcast pre-prepare as leader after change round")
				return
			}

			_, highest, e := highestPrepared(signedMessage.Message.Round, i.ChangeRoundMessages)
			if e != nil {
				err = e
				return
			}

			var value []byte
			if highest != nil {
				value = highest.PreparedValue
				i.Logger.Info("broadcasting pre-prepare as leader after round change with justified prepare value", zap.Uint64("round", signedMessage.Message.Round))

			} else {
				value = i.State.InputValue
				i.Logger.Info("broadcasting pre-prepare as leader after round change with input value", zap.Uint64("round", signedMessage.Message.Round))
			}

			// send pre-prepare msg
			broadcastMsg := i.generatePrePrepareMessage(value)
			if e := i.SignAndBroadcast(broadcastMsg); e != nil {
				i.Logger.Error("could not broadcast pre-prepare message after round change", zap.Error(err))
				err = e
			}
		})
		return err
	})
}

// JustifyRoundChange see below
func (i *Instance) JustifyRoundChange(round uint64) (bool, error) {
	// ### Algorithm 4 IBFT pseudocode for process pi: message justification
	//	predicate JustifyRoundChange(Qrc) return
	//		∀⟨ROUND-CHANGE, λi, ri, prj, pvj⟩ ∈ Qrc : prj = ⊥ ∧ pvj = ⊥
	//		∨ received a quorum of valid ⟨PREPARE, λi, pr, pv⟩ messages such that:
	//			(pr, pv) = HighestPrepared(Qrc)

	if quorum, _, _ := i.changeRoundQuorum(round); !quorum {
		return false, nil
	}
	justifiedNotPrepapred, data, err := highestPrepared(round, i.ChangeRoundMessages)
	if err != nil {
		return false, err
	}
	if justifiedNotPrepapred || data == nil { // all change round messages have prj = ⊥ ∧ pvj = ⊥
		return true, nil
	}

	// we've received a justification for a prepared round and value.
	return true, nil
}

func (i *Instance) changeRoundQuorum(round uint64) (quorum bool, t int, n int) {
	// TODO - calculate quorum one way (for prepare, commit, change round and decided) and refactor
	msgs := i.ChangeRoundMessages.ReadOnlyMessagesByRound(round)
	quorum = len(msgs)*3 >= i.ValidatorShare.CommitteeSize()*2
	return quorum, len(msgs), i.ValidatorShare.CommitteeSize()
}

func (i *Instance) roundChangeInputValue() ([]byte, error) {
	quorum, msgs := i.PrepareMessages.QuorumAchieved(i.State.PreparedRound, i.State.PreparedValue)

	// prepare justificationMsg and sig
	var justificationMsg *proto.Message
	var aggSig []byte
	ids := make([]uint64, 0)
	if quorum {
		var aggregatedSig *bls.Sign
		justificationMsg = msgs[0].Message
		for _, msg := range msgs {
			// add sig to aggregate
			sig := &bls.Sign{}
			if err := sig.Deserialize(msg.Signature); err != nil {
				return nil, err
			}
			if aggregatedSig == nil {
				aggregatedSig = sig
			} else {
				aggregatedSig.Add(sig)
			}

			// add id to list
			ids = append(ids, msg.SignerIds...)
		}
		aggSig = aggregatedSig.Serialize()
	}

	data := &proto.ChangeRoundData{
		PreparedRound:    i.State.PreparedRound,
		PreparedValue:    i.State.PreparedValue,
		JustificationMsg: justificationMsg,
		JustificationSig: aggSig,
		SignerIds:        ids,
	}

	return json.Marshal(data)
}

func (i *Instance) uponChangeRoundTrigger() {
	i.Logger.Info("round timeout, changing round", zap.Uint64("round", i.Round()))
	// bump round
	i.BumpRound()
	// mark stage
	i.ProcessStageChange(proto.RoundState_ChangeRound)

	// set time for next round change
	i.resetRoundTimer()
	// broadcast round change
	if err := i.broadcastChangeRound(); err != nil {
		i.Logger.Error("could not broadcast round change message", zap.Error(err))
	}
}

func (i *Instance) broadcastChangeRound() error {
	broadcastMsg, err := i.generateChangeRoundMessage()
	if err != nil {
		return err
	}
	if err := i.SignAndBroadcast(broadcastMsg); err != nil {
		return err
	}
	i.Logger.Info("broadcasted change round", zap.Uint64("round", broadcastMsg.Round))
	return nil
}

/**
### Algorithm 4 IBFT pseudocode for process pi: message justification
	Helper function that returns a tuple (pr, pv) where pr and pv are, respectively,
	the prepared round and the prepared value of the ROUND-CHANGE message in Qrc with the highest prepared round.
	function HighestPrepared(Qrc)
		return (pr, pv) such that:
			∃⟨ROUND-CHANGE, λi, round, pr, pv⟩ ∈ Qrc :
				∀⟨ROUND-CHANGE, λi, round, prj, pvj⟩ ∈ Qrc : prj = ⊥ ∨ pr ≥ prj
*/
// highestPrepared is slightly changed to also include a returned flag to indicate if all change round messages have prj = ⊥ ∧ pvj = ⊥
func highestPrepared(round uint64, container msgcont.MessageContainer) (allNonPrepared bool, changeData *proto.ChangeRoundData, err error) {
	allNonPrepared = true
	for _, msg := range container.ReadOnlyMessagesByRound(round) {
		candidateChangeData := &proto.ChangeRoundData{}
		err = json.Unmarshal(msg.Message.Value, candidateChangeData)
		if err != nil {
			return false, nil, err
		}

		if candidateChangeData.PreparedValue != nil {
			allNonPrepared = false
			// compare to highest found
			if changeData != nil {
				if candidateChangeData.PreparedRound > changeData.PreparedRound {
					changeData = candidateChangeData
				}
			} else {
				changeData = candidateChangeData
			}
		}
	}
	return allNonPrepared, changeData, nil
}

func (i *Instance) generateChangeRoundMessage() (*proto.Message, error) {
	data, err := i.roundChangeInputValue()
	if err != nil {
		//i.Logger.Error("failed to create round change data for round", zap.Uint64("round", i.Round()), zap.Error(err))
		return nil, errors.New("failed to create round change data for round")
	}

	return &proto.Message{
		Type:      proto.RoundState_ChangeRound,
		Round:     i.Round(),
		Lambda:    i.State.Lambda.Get(),
		SeqNumber: i.State.SeqNumber,
		Value:     data,
	}, nil
}

func (i *Instance) roundTimeoutSeconds() time.Duration {
	roundTimeout := math.Pow(float64(i.Config.RoundChangeDurationSeconds), float64(i.Round()))
	return time.Duration(float64(time.Second) * roundTimeout)
}
