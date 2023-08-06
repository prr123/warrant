// Copyright 2023 Forerunner Labs, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package authz

import (
	"context"

	objecttype "github.com/warrant-dev/warrant/pkg/authz/objecttype"
	"github.com/warrant-dev/warrant/pkg/event"
	"github.com/warrant-dev/warrant/pkg/service"
)

type WarrantService struct {
	service.BaseService
	Repository    WarrantRepository
	EventSvc      event.Service
	ObjectTypeSvc *objecttype.ObjectTypeService
}

func NewService(env service.Env, repository WarrantRepository, eventSvc event.Service, objectTypeSvc *objecttype.ObjectTypeService) *WarrantService {
	return &WarrantService{
		BaseService:   service.NewBaseService(env),
		Repository:    repository,
		EventSvc:      eventSvc,
		ObjectTypeSvc: objectTypeSvc,
	}
}

func (svc WarrantService) Create(ctx context.Context, warrantSpec WarrantSpec) (*WarrantSpec, error) {
	var createdWarrant Model
	err := svc.Env().DB().WithinTransaction(ctx, func(txCtx context.Context) error {
		// Check that objectType is valid
		objectTypeDef, err := svc.ObjectTypeSvc.GetByTypeId(txCtx, warrantSpec.ObjectType)
		if err != nil {
			return service.NewInvalidParameterError("objectType", "The given object type does not exist.")
		}

		// Check that relation is valid for objectType
		_, exists := objectTypeDef.Relations[warrantSpec.Relation]
		if !exists {
			return service.NewInvalidParameterError("relation", "An object type with the given relation does not exist.")
		}

		// Check that warrant does not already exist
		_, err = svc.Repository.Get(txCtx, warrantSpec.ObjectType, warrantSpec.ObjectId, warrantSpec.Relation, warrantSpec.Subject.ObjectType, warrantSpec.Subject.ObjectId, warrantSpec.Subject.Relation, warrantSpec.Policy.Hash())
		if err == nil {
			return service.NewDuplicateRecordError("Warrant", warrantSpec, "A warrant with the given objectType, objectId, relation, subject, and policy already exists")
		}

		warrant, err := warrantSpec.ToWarrant()
		if err != nil {
			return err
		}

		createdWarrantId, err := svc.Repository.Create(txCtx, warrant)
		if err != nil {
			return err
		}

		createdWarrant, err = svc.Repository.GetByID(txCtx, createdWarrantId)
		if err != nil {
			return err
		}

		var eventMeta map[string]interface{}
		if createdWarrant.GetPolicy() != "" {
			eventMeta = make(map[string]interface{})
			eventMeta["policy"] = createdWarrant.GetPolicy()
		}

		err = svc.EventSvc.TrackAccessGrantedEvent(txCtx, createdWarrant.GetObjectType(), createdWarrant.GetObjectId(), createdWarrant.GetRelation(), createdWarrant.GetSubjectType(), createdWarrant.GetSubjectId(), createdWarrant.GetSubjectRelation(), eventMeta)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return createdWarrant.ToWarrantSpec(), nil
}

func (svc WarrantService) List(ctx context.Context, filterOptions *FilterOptions, listParams service.ListParams) ([]*WarrantSpec, error) {
	warrantSpecs := make([]*WarrantSpec, 0)
	warrants, err := svc.Repository.List(ctx, filterOptions, listParams)
	if err != nil {
		return warrantSpecs, err
	}

	for _, warrant := range warrants {
		warrantSpecs = append(warrantSpecs, warrant.ToWarrantSpec())
	}

	return warrantSpecs, nil
}

func (svc WarrantService) Delete(ctx context.Context, warrantSpec WarrantSpec) error {
	err := svc.Env().DB().WithinTransaction(ctx, func(txCtx context.Context) error {
		warrantToDelete, err := warrantSpec.ToWarrant()
		if err != nil {
			return nil
		}

		_, err = svc.Repository.Get(txCtx, warrantToDelete.GetObjectType(), warrantToDelete.GetObjectId(), warrantToDelete.GetRelation(), warrantToDelete.GetSubjectType(), warrantToDelete.GetSubjectId(), warrantToDelete.GetSubjectRelation(), warrantToDelete.GetPolicyHash())
		if err != nil {
			return err
		}

		err = svc.Repository.Delete(txCtx, warrantToDelete.GetObjectType(), warrantToDelete.GetObjectId(), warrantToDelete.GetRelation(), warrantToDelete.GetSubjectType(), warrantToDelete.GetSubjectId(), warrantToDelete.GetSubjectRelation(), warrantToDelete.GetPolicyHash())
		if err != nil {
			return err
		}

		var eventMeta map[string]interface{}
		if warrantSpec.Policy != "" {
			eventMeta = make(map[string]interface{})
			eventMeta["policy"] = warrantSpec.Policy
		}

		err = svc.EventSvc.TrackAccessRevokedEvent(txCtx, warrantSpec.ObjectType, warrantSpec.ObjectId, warrantSpec.Relation, warrantSpec.Subject.ObjectType, warrantSpec.Subject.ObjectId, warrantSpec.Subject.Relation, eventMeta)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (svc WarrantService) DeleteRelatedWarrants(ctx context.Context, objectType string, objectId string) error {
	err := svc.Env().DB().WithinTransaction(ctx, func(txCtx context.Context) error {
		warrantIdsToDelete := make([]int64, 0)
		accessRevokedEvents := make([]event.CreateAccessEventSpec, 0)
		warrantsMatchingObject, err := svc.Repository.GetAllMatchingObject(txCtx, objectType, objectId)
		if err != nil {
			return err
		}

		for _, warrant := range warrantsMatchingObject {
			warrantIdsToDelete = append(warrantIdsToDelete, warrant.GetID())
			accessRevokedEvents = append(accessRevokedEvents, event.CreateAccessEventSpec{
				Type:            event.EventTypeAccessRevoked,
				Source:          event.EventSourceApi,
				ObjectType:      warrant.GetObjectType(),
				ObjectId:        warrant.GetObjectId(),
				Relation:        warrant.GetRelation(),
				SubjectType:     warrant.GetSubjectType(),
				SubjectId:       warrant.GetSubjectId(),
				SubjectRelation: warrant.GetSubjectRelation(),
				Meta: map[string]interface{}{
					"policy": warrant.GetPolicy(),
				},
			})
		}

		warrantsMatchingSubject, err := svc.Repository.GetAllMatchingSubject(txCtx, objectType, objectId)
		if err != nil {
			return err
		}

		for _, warrant := range warrantsMatchingSubject {
			warrantIdsToDelete = append(warrantIdsToDelete, warrant.GetID())
			accessRevokedEvents = append(accessRevokedEvents, event.CreateAccessEventSpec{
				Type:            event.EventTypeAccessRevoked,
				Source:          event.EventSourceApi,
				ObjectType:      warrant.GetObjectType(),
				ObjectId:        warrant.GetObjectId(),
				Relation:        warrant.GetRelation(),
				SubjectType:     warrant.GetSubjectType(),
				SubjectId:       warrant.GetSubjectId(),
				SubjectRelation: warrant.GetSubjectRelation(),
				Meta: map[string]interface{}{
					"policy": warrant.GetPolicy(),
				},
			})
		}

		if len(warrantIdsToDelete) > 0 {
			err = svc.Repository.DeleteById(ctx, warrantIdsToDelete)
			if err != nil {
				return err
			}

			err = svc.EventSvc.TrackAccessEvents(ctx, accessRevokedEvents)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
