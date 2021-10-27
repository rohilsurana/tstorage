package tstorage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_fileWAL_append_read(t *testing.T) {
	type args struct {
		op   walOperation
		rows []Row
	}
	tests := []struct {
		name    string
		args    args
		want    []Row
		wantErr bool
	}{
		{
			name: "successfully append and read",
			args: args{
				op: operationInsert,
				rows: []Row{
					{
						Metric: "metric-1",
						DataPoint: DataPoint{
							Value:     0.1,
							Timestamp: 1600000000,
						},
					},
					{
						Metric: "metric-2",
						DataPoint: DataPoint{
							Value:     0.2,
							Timestamp: 1600000001,
						},
					},
				},
			},
			want: []Row{
				{
					Metric: "metric-1",
					DataPoint: DataPoint{
						Value:     0.1,
						Timestamp: 1600000000,
					},
				},
				{
					Metric: "metric-2",
					DataPoint: DataPoint{
						Value:     0.2,
						Timestamp: 1600000001,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Append rows into wal
			tmpDir, err := os.MkdirTemp("", "tstorage-test")
			require.NoError(t, err)
			path := filepath.Join(tmpDir, "wal")

			wal, err := newDiskWAL(path, 4096)
			require.NoError(t, err)

			err = wal.append(tt.args.op, tt.args.rows)
			require.NoError(t, err)

			err = wal.flush()
			require.NoError(t, err)

			// Read all wal rows.
			reader, err := newDiskWALReader(path)
			require.NoError(t, err)
			err = reader.readAll()
			require.NoError(t, err)
			got := reader.rowsToInsert
			assert.Equal(t, got, tt.want)
		})
	}
}