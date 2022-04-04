package round

import (
	"github.com/bloxapp/ssv/protocol/v1/message"
	protocolp2p "github.com/bloxapp/ssv/protocol/v1/p2p"
	qbftstorage "github.com/bloxapp/ssv/protocol/v1/qbft/storage"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// LastChangeRoundHandler handler for last-decided protocol
// TODO: add msg validation
func LastChangeRoundHandler(plogger *zap.Logger, store qbftstorage.InstanceStore) protocolp2p.RequestHandler {
	plogger = plogger.With(zap.String("who", "last decided handler"))
	return func(msg *message.SSVMessage) (*message.SSVMessage, error) {
		//logger := plogger.With(zap.String("msg_id_hex", fmt.Sprintf("%x", msg.ID)))
		sm := &message.SyncMessage{}
		err := sm.Decode(msg.Data)
		if err != nil {
			return nil, errors.Wrap(err, "could not decode msg data")
		}
		res, err := store.GetLastChangeRoundMsg(msg.ID)
		sm.UpdateResults(err, res)

		data, err := sm.Encode()
		if err != nil {
			return nil, errors.Wrap(err, "could not encode result data")
		}
		msg.Data = data

		return msg, nil
	}
}
