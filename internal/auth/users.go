// Copyright (C) 2022 NHR@FAU, University Erlangen-Nuremberg.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.
package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ClusterCockpit/cc-backend/internal/graph/model"
	"github.com/ClusterCockpit/cc-backend/pkg/log"
	sq "github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

func (auth *Authentication) GetUser(username string) (*User, error) {

	user := &User{Username: username}
	var hashedPassword, name, rawRoles, email, rawProjects sql.NullString
	if err := sq.Select("password", "ldap", "name", "roles", "email", "projects").From("user").
		Where("user.username = ?", username).RunWith(auth.db).
		QueryRow().Scan(&hashedPassword, &user.AuthSource, &name, &rawRoles, &email, &rawProjects); err != nil {
		log.Warnf("Error while querying user '%v' from database", username)
		return nil, err
	}

	user.Password = hashedPassword.String
	user.Name = name.String
	user.Email = email.String
	if rawRoles.Valid {
		if err := json.Unmarshal([]byte(rawRoles.String), &user.Roles); err != nil {
			log.Warn("Error while unmarshaling raw roles from DB")
			return nil, err
		}
	}
	if rawProjects.Valid {
		if err := json.Unmarshal([]byte(rawProjects.String), &user.Projects); err != nil {
			return nil, err
		}
	}

	return user, nil
}

func (auth *Authentication) AddUser(user *User) error {

	rolesJson, _ := json.Marshal(user.Roles)
	projectsJson, _ := json.Marshal(user.Projects)

	cols := []string{"username", "roles", "projects"}
	vals := []interface{}{user.Username, string(rolesJson), string(projectsJson)}

	if user.Name != "" {
		cols = append(cols, "name")
		vals = append(vals, user.Name)
	}
	if user.Email != "" {
		cols = append(cols, "email")
		vals = append(vals, user.Email)
	}
	if user.Password != "" {
		password, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Error("Error while encrypting new user password")
			return err
		}
		cols = append(cols, "password")
		vals = append(vals, string(password))
	}

	if _, err := sq.Insert("user").Columns(cols...).Values(vals...).RunWith(auth.db).Exec(); err != nil {
		log.Errorf("Error while inserting new user '%v' into DB", user.Username)
		return err
	}

	log.Infof("new user %#v created (roles: %s, auth-source: %d, projects: %s)", user.Username, rolesJson, user.AuthSource, projectsJson)
	return nil
}

func (auth *Authentication) DelUser(username string) error {

	_, err := auth.db.Exec(`DELETE FROM user WHERE user.username = ?`, username)
	log.Errorf("Error while deleting user '%s' from DB", username)
	return err
}

func (auth *Authentication) ListUsers(specialsOnly bool) ([]*User, error) {

	q := sq.Select("username", "name", "email", "roles", "projects").From("user")
	if specialsOnly {
		q = q.Where("(roles != '[\"user\"]' AND roles != '[]')")
	}

	rows, err := q.RunWith(auth.db).Query()
	if err != nil {
		log.Warn("Error while querying user list")
		return nil, err
	}

	users := make([]*User, 0)
	defer rows.Close()
	for rows.Next() {
		rawroles := ""
		rawprojects := ""
		user := &User{}
		var name, email sql.NullString
		if err := rows.Scan(&user.Username, &name, &email, &rawroles, &rawprojects); err != nil {
			log.Warn("Error while scanning user list")
			return nil, err
		}

		if err := json.Unmarshal([]byte(rawroles), &user.Roles); err != nil {
			log.Warn("Error while unmarshaling raw role list")
			return nil, err
		}

		if err := json.Unmarshal([]byte(rawprojects), &user.Projects); err != nil {
			return nil, err
		}

		user.Name = name.String
		user.Email = email.String
		users = append(users, user)
	}
	return users, nil
}

func (auth *Authentication) AddRole(
	ctx context.Context,
	username string,
	queryrole string) error {

	newRole := strings.ToLower(queryrole)
	user, err := auth.GetUser(username)
	if err != nil {
		log.Warnf("Could not load user '%s'", username)
		return err
	}

	exists, valid := user.HasValidRole(newRole)

	if !valid {
		return fmt.Errorf("Supplied role is no valid option : %v", newRole)
	}
	if exists {
		return fmt.Errorf("User %v already has role %v", username, newRole)
	}

	roles, _ := json.Marshal(append(user.Roles, newRole))
	if _, err := sq.Update("user").Set("roles", roles).Where("user.username = ?", username).RunWith(auth.db).Exec(); err != nil {
		log.Errorf("Error while adding new role for user '%s'", user.Username)
		return err
	}
	return nil
}

func (auth *Authentication) RemoveRole(ctx context.Context, username string, queryrole string) error {
	oldRole := strings.ToLower(queryrole)
	user, err := auth.GetUser(username)
	if err != nil {
		log.Warnf("Could not load user '%s'", username)
		return err
	}

	exists, valid := user.HasValidRole(oldRole)

	if !valid {
		return fmt.Errorf("Supplied role is no valid option : %v", oldRole)
	}
	if !exists {
		return fmt.Errorf("Role already deleted for user '%v': %v", username, oldRole)
	}

	if oldRole == GetRoleString(RoleManager) && len(user.Projects) != 0 {
		return fmt.Errorf("Cannot remove role 'manager' while user %s still has assigned project(s) : %v", username, user.Projects)
	}

	var newroles []string
	for _, r := range user.Roles {
		if r != oldRole {
			newroles = append(newroles, r) // Append all roles not matching requested to be deleted role
		}
	}

	var mroles, _ = json.Marshal(newroles)
	if _, err := sq.Update("user").Set("roles", mroles).Where("user.username = ?", username).RunWith(auth.db).Exec(); err != nil {
		log.Errorf("Error while removing role for user '%s'", user.Username)
		return err
	}
	return nil
}

func (auth *Authentication) AddProject(
	ctx context.Context,
	username string,
	project string) error {

	user, err := auth.GetUser(username)
	if err != nil {
		return err
	}

	if !user.HasRole(RoleManager) {
		return fmt.Errorf("user '%s' is not a manager!", username)
	}

	if user.HasProject(project) {
		return fmt.Errorf("user '%s' already manages project '%s'", username, project)
	}

	projects, _ := json.Marshal(append(user.Projects, project))
	if _, err := sq.Update("user").Set("projects", projects).Where("user.username = ?", username).RunWith(auth.db).Exec(); err != nil {
		return err
	}

	return nil
}

func (auth *Authentication) RemoveProject(ctx context.Context, username string, project string) error {
	user, err := auth.GetUser(username)
	if err != nil {
		return err
	}

	if !user.HasRole(RoleManager) {
		return fmt.Errorf("user '%#v' is not a manager!", username)
	}

	if !user.HasProject(project) {
		return fmt.Errorf("user '%#v': Cannot remove project '%#v' - Does not match!", username, project)
	}

	var exists bool
	var newprojects []string
	for _, p := range user.Projects {
		if p != project {
			newprojects = append(newprojects, p) // Append all projects not matching requested to be deleted project
		} else {
			exists = true
		}
	}

	if exists == true {
		var result interface{}
		if len(newprojects) == 0 {
			result = "[]"
		} else {
			result, _ = json.Marshal(newprojects)
		}
		if _, err := sq.Update("user").Set("projects", result).Where("user.username = ?", username).RunWith(auth.db).Exec(); err != nil {
			return err
		}
		return nil
	} else {
		return fmt.Errorf("user %s already does not manage project %s", username, project)
	}
}

func FetchUser(ctx context.Context, db *sqlx.DB, username string) (*model.User, error) {
	me := GetUser(ctx)
	if me != nil && me.Username != username && me.HasNotRoles([]Role{RoleAdmin, RoleSupport, RoleManager}) {
		return nil, errors.New("forbidden")
	}

	user := &model.User{Username: username}
	var name, email sql.NullString
	if err := sq.Select("name", "email").From("user").Where("user.username = ?", username).
		RunWith(db).QueryRow().Scan(&name, &email); err != nil {
		if err == sql.ErrNoRows {
			/* This warning will be logged *often* for non-local users, i.e. users mentioned only in job-table or archive, */
			/* since FetchUser will be called to retrieve full name and mail for every job in query/list									 */
			// log.Warnf("User '%s' Not found in DB", username)
			return nil, nil
		}

		log.Warnf("Error while fetching user '%s'", username)
		return nil, err
	}

	user.Name = name.String
	user.Email = email.String
	return user, nil
}
