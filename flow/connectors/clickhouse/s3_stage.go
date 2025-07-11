package connclickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/PeerDB-io/peerdb/flow/connectors/utils"
	"github.com/PeerDB-io/peerdb/flow/internal"
)

func SetAvroStage(
	ctx context.Context,
	flowJobName string,
	syncBatchID int64,
	avroFile utils.AvroFile,
) error {
	avroFileJSON, err := json.Marshal(avroFile)
	if err != nil {
		return fmt.Errorf("failed to marshal avro file: %w", err)
	}

	conn, err := internal.GetCatalogConnectionPoolFromEnv(ctx)
	if err != nil {
		return fmt.Errorf("failed to get connection: %w", err)
	}

	if _, err := conn.Exec(ctx, `
		INSERT INTO ch_s3_stage (flow_job_name, sync_batch_id, avro_file)
		VALUES ($1, $2, $3)
		ON CONFLICT (flow_job_name, sync_batch_id)
		DO UPDATE SET avro_file = $3, created_at = CURRENT_TIMESTAMP`,
		flowJobName, syncBatchID, avroFileJSON,
	); err != nil {
		return fmt.Errorf("failed to set avro stage: %w", err)
	}

	return nil
}

func GetAvroStage(ctx context.Context, flowJobName string, syncBatchID int64) (utils.AvroFile, error) {
	conn, err := internal.GetCatalogConnectionPoolFromEnv(ctx)
	if err != nil {
		return utils.AvroFile{}, fmt.Errorf("failed to get connection: %w", err)
	}

	var avroFileJSON []byte
	if err := conn.QueryRow(ctx, `
		SELECT avro_file FROM ch_s3_stage
		WHERE flow_job_name = $1 AND sync_batch_id = $2`,
		flowJobName, syncBatchID,
	).Scan(&avroFileJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return utils.AvroFile{}, fmt.Errorf("no avro stage found for flow job %s and sync batch %d", flowJobName, syncBatchID)
		}
		return utils.AvroFile{}, fmt.Errorf("failed to get avro stage: %w", err)
	}

	var avroFile utils.AvroFile
	if err := json.Unmarshal(avroFileJSON, &avroFile); err != nil {
		return utils.AvroFile{}, fmt.Errorf("failed to unmarshal avro file: %w", err)
	}

	return avroFile, nil
}
