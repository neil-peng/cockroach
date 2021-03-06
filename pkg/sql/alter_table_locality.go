// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/dbdesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/errors"
)

type alterTableSetLocalityNode struct {
	n         tree.AlterTableLocality
	tableDesc *tabledesc.Mutable
	dbDesc    *dbdesc.Immutable
}

// AlterTableLocality transforms a tree.AlterTableLocality into a plan node.
func (p *planner) AlterTableLocality(
	ctx context.Context, n *tree.AlterTableLocality,
) (planNode, error) {
	if err := checkSchemaChangeEnabled(
		ctx,
		p.ExecCfg(),
		"ALTER TABLE",
	); err != nil {
		return nil, err
	}

	tableDesc, err := p.ResolveMutableTableDescriptorEx(
		ctx, n.Name, !n.IfExists, tree.ResolveRequireTableDesc,
	)
	if err != nil {
		return nil, err
	}
	if tableDesc == nil {
		return newZeroNode(nil /* columns */), nil
	}

	// This check for CREATE privilege is kept for backwards compatibility.
	if err := p.CheckPrivilege(ctx, tableDesc, privilege.CREATE); err != nil {
		return nil, pgerror.Newf(pgcode.InsufficientPrivilege,
			"must be owner of table %s or have CREATE privilege on table %s",
			tree.Name(tableDesc.GetName()), tree.Name(tableDesc.GetName()))
	}

	// Ensure that the database is multi-region enabled.
	dbDesc, err := p.Descriptors().GetImmutableDatabaseByID(
		ctx,
		p.txn,
		tableDesc.GetParentID(),
		tree.DatabaseLookupFlags{},
	)
	if err != nil {
		return nil, err
	}

	if !dbDesc.IsMultiRegion() {
		return nil, pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"cannot alter a table's LOCALITY if its database is not multi-region enabled",
		)
	}

	return &alterTableSetLocalityNode{
		n:         *n,
		tableDesc: tableDesc,
		dbDesc:    dbDesc,
	}, nil
}

func (n *alterTableSetLocalityNode) Next(runParams) (bool, error) { return false, nil }
func (n *alterTableSetLocalityNode) Values() tree.Datums          { return tree.Datums{} }
func (n *alterTableSetLocalityNode) Close(context.Context)        {}

func (n *alterTableSetLocalityNode) alterTableLocalityGlobalToRegionalByTable(
	params runParams,
) error {
	if !n.tableDesc.IsLocalityGlobal() {
		f := tree.NewFmtCtx(tree.FmtSimple)
		if err := tabledesc.FormatTableLocalityConfig(n.tableDesc.LocalityConfig, f); err != nil {
			// While we're in an error path and generally it's bad to return a
			// different error in an error path, we will only get an error here if the
			// locality is corrupted, in which case, it's probably the right error
			// to return.
			return err
		}
		return errors.AssertionFailedf(
			"invalid call %q on incorrect table locality %s",
			"alter table locality GLOBAL to REGIONAL BY TABLE",
			f.String(),
		)
	}

	n.tableDesc.SetTableLocalityRegionalByTable(n.n.Locality.TableRegion)

	// Finalize the alter by writing a new table descriptor and updating the zone
	// configuration.
	if err := n.validateAndWriteNewTableLocalityAndZoneConfig(
		params,
		n.dbDesc,
	); err != nil {
		return err
	}

	return nil
}

func (n *alterTableSetLocalityNode) alterTableLocalityRegionalByTableToGlobal(
	params runParams,
) error {
	const operation string = "alter table locality REGIONAL BY TABLE to GLOBAL"
	if !n.tableDesc.IsLocalityRegionalByTable() {
		return errors.AssertionFailedf(
			"invalid call %q on incorrect table locality. %v",
			operation,
			n.tableDesc.LocalityConfig,
		)
	}

	n.tableDesc.SetTableLocalityGlobal()

	// Finalize the alter by writing a new table descriptor and updating the zone
	// configuration.
	if err := n.validateAndWriteNewTableLocalityAndZoneConfig(
		params,
		n.dbDesc,
	); err != nil {
		return err
	}

	return nil
}

func (n *alterTableSetLocalityNode) alterTableLocalityRegionalByTableToRegionalByTable(
	params runParams,
) error {
	const operation string = "alter table locality REGIONAL BY TABLE to REGIONAL BY TABLE"
	if !n.tableDesc.IsLocalityRegionalByTable() {
		return errors.AssertionFailedf(
			"invalid call %q on incorrect table locality. %v",
			operation,
			n.tableDesc.LocalityConfig,
		)
	}

	n.tableDesc.SetTableLocalityRegionalByTable(n.n.Locality.TableRegion)

	// Finalize the alter by writing a new table descriptor and updating the zone configuration.
	if err := n.validateAndWriteNewTableLocalityAndZoneConfig(
		params,
		n.dbDesc,
	); err != nil {
		return err
	}

	return nil
}

func (n *alterTableSetLocalityNode) alterTableLocalityNonRegionalByRowToRegionalByRow(
	params runParams,
	existingLocality *descpb.TableDescriptor_LocalityConfig,
	newLocality *tree.Locality,
) error {
	if newLocality.RegionalByRowColumn == tree.RegionalByRowRegionNotSpecifiedName {
		return unimplemented.NewWithIssue(59632, "implementation pending")
	}

	// Ensure column exists and is of the correct type.
	partCol, _, err := n.tableDesc.FindColumnByName(newLocality.RegionalByRowColumn)
	if err != nil {
		return err
	}
	enumTypeID, err := n.dbDesc.MultiRegionEnumID()
	if err != nil {
		return err
	}
	if partCol.Type.Oid() != typedesc.TypeIDToOID(enumTypeID) {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"cannot use column %s for REGIONAL BY ROW as it does not have the %s type",
			newLocality.RegionalByRowColumn,
			tree.RegionEnum,
		)
	}

	// Preserve the same PK columns - implicit partitioning will be added in
	// AlterPrimaryKey.
	cols := make([]tree.IndexElem, len(n.tableDesc.PrimaryIndex.ColumnNames))
	for i, col := range n.tableDesc.PrimaryIndex.ColumnNames {
		cols[i] = tree.IndexElem{
			Column: tree.Name(col),
		}
		switch dir := n.tableDesc.PrimaryIndex.ColumnDirections[i]; dir {
		case descpb.IndexDescriptor_ASC:
			cols[i].Direction = tree.Ascending
		case descpb.IndexDescriptor_DESC:
			cols[i].Direction = tree.Descending
		default:
			return errors.AssertionFailedf("unknown direction: %v", dir)
		}
	}

	// We re-use ALTER PRIMARY KEY to do the the work for us.
	//
	// Altering to REGIONAL BY ROW is effectively a PRIMARY KEY swap where we
	// add the implicit partitioning to the PK, with all indexes underneath
	// being re-written to point to the correct PRIMARY KEY and also being
	// implicitly partitioned. The AlterPrimaryKey will also set the relevant
	// zone configurations appropriate stages of the newly re-created indexes
	// on the table itself.
	if err := params.p.AlterPrimaryKey(
		params.ctx,
		n.tableDesc,
		&tree.AlterTableAlterPrimaryKey{
			Name:    tree.Name(n.tableDesc.PrimaryIndex.Name),
			Columns: cols,
		},
		&descpb.PrimaryKeySwap_LocalityConfigSwap{
			OldLocalityConfig: *existingLocality,
			NewLocalityConfig: tabledesc.LocalityConfigRegionalByRow(
				newLocality.RegionalByRowColumn,
			),
		},
	); err != nil {
		return err
	}

	return params.p.writeSchemaChange(
		params.ctx,
		n.tableDesc,
		n.tableDesc.ClusterVersion.NextMutationID,
		tree.AsStringWithFQNames(&n.n, params.Ann()),
	)
}

func (n *alterTableSetLocalityNode) startExec(params runParams) error {
	newLocality := n.n.Locality
	existingLocality := n.tableDesc.LocalityConfig

	// Look at the existing locality, and implement any changes required to move to
	// the new locality.
	switch existingLocality.Locality.(type) {
	case *descpb.TableDescriptor_LocalityConfig_Global_:
		switch newLocality.LocalityLevel {
		case tree.LocalityLevelGlobal:
			return nil
		case tree.LocalityLevelRow:
			if err := n.alterTableLocalityNonRegionalByRowToRegionalByRow(
				params,
				existingLocality,
				newLocality,
			); err != nil {
				return err
			}
		case tree.LocalityLevelTable:
			if err := n.alterTableLocalityGlobalToRegionalByTable(params); err != nil {
				return err
			}
		default:
			return errors.AssertionFailedf("unknown table locality: %v", newLocality)
		}
	case *descpb.TableDescriptor_LocalityConfig_RegionalByTable_:
		switch newLocality.LocalityLevel {
		case tree.LocalityLevelGlobal:
			if err := n.alterTableLocalityRegionalByTableToGlobal(params); err != nil {
				return err
			}
		case tree.LocalityLevelRow:
			if err := n.alterTableLocalityNonRegionalByRowToRegionalByRow(
				params,
				existingLocality,
				newLocality,
			); err != nil {
				return err
			}
		case tree.LocalityLevelTable:
			if err := n.alterTableLocalityRegionalByTableToRegionalByTable(params); err != nil {
				return err
			}
		default:
			return errors.AssertionFailedf("unknown table locality: %v", newLocality)
		}
	case *descpb.TableDescriptor_LocalityConfig_RegionalByRow_:
		switch newLocality.LocalityLevel {
		case tree.LocalityLevelGlobal:
			return unimplemented.NewWithIssue(59632, "implementation pending")
		case tree.LocalityLevelRow:
			return unimplemented.New("alter table locality from REGIONAL BY ROW", "implementation pending")
		case tree.LocalityLevelTable:
			return unimplemented.NewWithIssue(59632, "implementation pending")
		default:
			return errors.AssertionFailedf("unknown table locality: %v", newLocality)
		}
	default:
		return errors.AssertionFailedf("unknown table locality: %v", existingLocality)
	}

	// Record this table alteration in the event log. This is an auditable log
	// event and is recorded in the same transaction as the table descriptor
	// update.
	return params.p.logEvent(params.ctx,
		n.tableDesc.ID,
		&eventpb.AlterTable{
			TableName: n.n.Name.String(),
		})
}

// validateAndWriteNewTableLocalityAndZoneConfig validates the newly updated
// LocalityConfig in a table descriptor, writes that table descriptor, and
// writes a new zone configuration for the given table.
func (n *alterTableSetLocalityNode) validateAndWriteNewTableLocalityAndZoneConfig(
	params runParams, dbDesc *dbdesc.Immutable,
) error {
	// Validate the new locality before updating the table descriptor.
	dg := catalogkv.NewOneLevelUncachedDescGetter(params.p.txn, params.EvalContext().Codec)
	if err := n.tableDesc.ValidateTableLocalityConfig(
		params.ctx,
		dg,
	); err != nil {
		return err
	}

	// Write out the table descriptor update.
	if err := params.p.writeSchemaChange(
		params.ctx,
		n.tableDesc,
		descpb.InvalidMutationID,
		tree.AsStringWithFQNames(&n.n, params.Ann()),
	); err != nil {
		return err
	}

	// Update the zone configuration.
	if err := applyZoneConfigForMultiRegionTable(
		params.ctx,
		params.p.txn,
		params.p.ExecCfg(),
		*dbDesc.RegionConfig,
		n.tableDesc,
		applyZoneConfigForMultiRegionTableOptionTableAndIndexes,
	); err != nil {
		return err
	}

	return nil
}
