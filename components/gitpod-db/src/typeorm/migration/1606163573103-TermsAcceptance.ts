/**
 * Copyright (c) 2020 TypeFox GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

import {MigrationInterface, QueryRunner} from "typeorm";

export class TermsAcceptance1606163573103 implements MigrationInterface {

    public async up(queryRunner: QueryRunner): Promise<any> {
        await queryRunner.query("CREATE TABLE IF NOT EXISTS `d_b_terms_acceptance_entry` ( `userId` char(36) NOT NULL, `termsRevision` varchar(255) NOT NULL, `acceptionTime` varchar(255) NOT NULL, PRIMARY KEY (userId) ) ENGINE=InnoDB");
    }

    public async down(queryRunner: QueryRunner): Promise<any> {
    }

}
