package compactor

import (
	"context"

	"github.com/go-kit/log"

	"github.com/grafana/loki/pkg/storage/config"
	"github.com/grafana/loki/pkg/storage/stores/indexshipper/compactor"
)

const (
	batchSize = 1000
)

type indexEntry struct {
	k, v []byte
}

// IndexProcessor implements compactor.IndexProcessor
type IndexProcessor struct{}

// NewIndexCompactor returns a new IndexProcessor which implements compactor.IndexCompactor
func NewIndexCompactor() IndexProcessor {
	return IndexProcessor{}
}

func (i IndexProcessor) NewTableCompactor(ctx context.Context, commonIndexSet compactor.IndexSet, existingUserIndexSet map[string]compactor.IndexSet, userIndexSetFactoryFunc compactor.MakeEmptyUserIndexSetFunc, periodConfig config.PeriodConfig) compactor.TableCompactor {
	return newTableCompactor(ctx, commonIndexSet, existingUserIndexSet, userIndexSetFactoryFunc, periodConfig)
}

func (i IndexProcessor) OpenCompactedIndexFile(_ context.Context, path, tableName, _, workingDir string, periodConfig config.PeriodConfig, logger log.Logger) (compactor.CompactedIndex, error) {
	boltdb, err := openBoltdbFileWithNoSync(path)
	if err != nil {
		return nil, err
	}

	return newCompactedIndex(boltdb, tableName, workingDir, periodConfig, logger), nil
}
